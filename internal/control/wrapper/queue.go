package wrapper

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

const (
	StreamKey       = "control:wrapper:jobs:v1"
	GroupName       = "wrapper-workers"
	DLQKey          = "control:wrapper:jobs:dlq:v1"
	TerminalPrefix  = "control:wrapper:terminal:v1:"
	enqueuePrefix   = "control:wrapper:enqueued:v1:"
	leaseIDPrefix   = "control:wrapper:lease:id:v1:"
	leaseMsgPrefix  = "control:wrapper:lease:message:v1:"
	terminalTTL     = 7 * 24 * time.Hour
	maxLeaseSeconds = 15 * 60
	minClaimIdle    = time.Second
)

var enqueueScript = redis.NewScript(`
local existing = redis.call('GET', KEYS[1])
if existing then
  return existing
end
local id = redis.call('XADD', KEYS[2], '*', 'job_id', ARGV[1], 'payload', ARGV[2])
redis.call('SET', KEYS[1], id, 'EX', ARGV[3])
return id
`)

var terminalScript = redis.NewScript(`
local existing = redis.call('GET', KEYS[1])
if existing then
  if existing == ARGV[1] then return 1 end
  return -2
end
if tonumber(ARGV[5]) >= tonumber(ARGV[6]) then return -1 end
if redis.call('EXISTS', KEYS[2]) == 0 then return -1 end
if redis.call('GET', KEYS[3]) ~= ARGV[2] then return -1 end
redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[4], 'NX')
redis.call('XACK', KEYS[4], ARGV[3], ARGV[7])
redis.call('DEL', KEYS[2], KEYS[3])
return 0
`)

var (
	ErrLeaseLost        = errors.New("wrapper lease is no longer owned")
	ErrTerminalConflict = errors.New("wrapper job already has a different terminal result")
)

type Queue struct {
	Redis redis.UniversalClient
	Now   func() time.Time
}

type leasedMessage struct {
	MessageID string                 `json:"message_id"`
	Lease     contracts.WrapperLease `json:"lease"`
}

type terminalEnvelope struct {
	Status     string                       `json:"status"`
	Completion *contracts.WrapperCompletion `json:"completion,omitempty"`
	Failure    *contracts.WrapperFailure    `json:"failure,omitempty"`
}

func (q Queue) Enqueue(ctx context.Context, job contracts.WrapperJob) (string, error) {
	if q.Redis == nil {
		return "", errors.New("wrapper queue redis is nil")
	}
	if err := job.Validate(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(job)
	if err != nil {
		return "", err
	}
	return enqueueScript.Run(ctx, q.Redis, []string{enqueuePrefix + job.JobID, StreamKey}, job.JobID, string(payload), int64(terminalTTL/time.Second)).Text()
}

func (q Queue) Claim(ctx context.Context, claim contracts.WrapperClaim) (contracts.WrapperJob, contracts.WrapperLease, error) {
	if q.Redis == nil {
		return contracts.WrapperJob{}, contracts.WrapperLease{}, errors.New("wrapper queue redis is nil")
	}
	if err := claim.Validate(); err != nil {
		return contracts.WrapperJob{}, contracts.WrapperLease{}, err
	}
	if claim.LeaseTTLSeconds > maxLeaseSeconds {
		return contracts.WrapperJob{}, contracts.WrapperLease{}, fmt.Errorf("%w: wrapper lease exceeds %d seconds", contracts.ErrInvalidContract, maxLeaseSeconds)
	}
	if err := q.ensureGroup(ctx); err != nil {
		return contracts.WrapperJob{}, contracts.WrapperLease{}, err
	}
	for attempt := 0; attempt < 32; attempt++ {
		message, err := q.nextMessage(ctx, claim.WorkerID, claim.LeaseTTLSeconds)
		if err != nil {
			return contracts.WrapperJob{}, contracts.WrapperLease{}, err
		}
		if message == nil {
			return contracts.WrapperJob{}, contracts.WrapperLease{}, redis.Nil
		}
		job, err := decodeJob(*message)
		if err != nil {
			if deadLetterErr := q.deadLetter(ctx, *message, err); deadLetterErr != nil {
				return contracts.WrapperJob{}, contracts.WrapperLease{}, errors.Join(err, deadLetterErr)
			}
			continue
		}
		if exists, err := q.Redis.Exists(ctx, TerminalPrefix+job.JobID).Result(); err != nil {
			return contracts.WrapperJob{}, contracts.WrapperLease{}, err
		} else if exists > 0 {
			if err := q.Redis.XAck(ctx, StreamKey, GroupName, message.ID).Err(); err != nil {
				return contracts.WrapperJob{}, contracts.WrapperLease{}, err
			}
			continue
		}
		leaseID, err := newLeaseID()
		if err != nil {
			return contracts.WrapperJob{}, contracts.WrapperLease{}, err
		}
		lease := contracts.WrapperLease{
			SchemaVersion: contracts.SchemaVersion,
			JobID:         job.JobID,
			LeaseID:       leaseID,
			WorkerID:      claim.WorkerID,
			AccountID:     job.AccountID,
			FenceEpoch:    job.FenceEpoch,
			ExpiresAt:     q.now().Add(time.Duration(claim.LeaseTTLSeconds) * time.Second),
		}
		record, err := json.Marshal(leasedMessage{MessageID: message.ID, Lease: lease})
		if err != nil {
			return contracts.WrapperJob{}, contracts.WrapperLease{}, err
		}
		ttl := time.Duration(claim.LeaseTTLSeconds) * time.Second
		pipe := q.Redis.TxPipeline()
		pipe.Set(ctx, leaseIDPrefix+leaseID, record, ttl)
		pipe.Set(ctx, leaseMsgPrefix+message.ID, leaseID, ttl)
		if _, err := pipe.Exec(ctx); err != nil {
			return contracts.WrapperJob{}, contracts.WrapperLease{}, err
		}
		return job, lease, nil
	}
	return contracts.WrapperJob{}, contracts.WrapperLease{}, redis.Nil
}

func (q Queue) Complete(ctx context.Context, lease contracts.WrapperLease, completion contracts.WrapperCompletion) error {
	if err := completion.ValidateLease(lease); err != nil {
		return err
	}
	return q.terminal(ctx, lease, terminalEnvelope{Status: "completed", Completion: &completion})
}

func (q Queue) Fail(ctx context.Context, lease contracts.WrapperLease, failure contracts.WrapperFailure) error {
	if err := failure.ValidateLease(lease); err != nil {
		return err
	}
	return q.terminal(ctx, lease, terminalEnvelope{Status: "failed", Failure: &failure})
}

func (q Queue) Terminal(ctx context.Context, jobID string) ([]byte, error) {
	if q.Redis == nil || strings.TrimSpace(jobID) == "" {
		return nil, fmt.Errorf("%w: wrapper job_id is empty", contracts.ErrInvalidContract)
	}
	return q.Redis.Get(ctx, TerminalPrefix+jobID).Bytes()
}

func (q Queue) terminal(ctx context.Context, lease contracts.WrapperLease, terminal terminalEnvelope) error {
	if q.Redis == nil {
		return errors.New("wrapper queue redis is nil")
	}
	if err := lease.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(terminal)
	if err != nil {
		return err
	}
	terminalKey := TerminalPrefix + lease.JobID
	existing, err := q.Redis.Get(ctx, terminalKey).Bytes()
	if err == nil {
		if string(existing) != string(payload) {
			return ErrTerminalConflict
		}
		if !q.now().Before(lease.ExpiresAt) {
			return nil
		}
	} else if !errors.Is(err, redis.Nil) {
		return err
	}
	if !q.now().Before(lease.ExpiresAt) {
		return ErrLeaseLost
	}
	recordRaw, err := q.Redis.Get(ctx, leaseIDPrefix+lease.LeaseID).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			if len(existing) > 0 {
				return nil
			}
			return ErrLeaseLost
		}
		return err
	}
	var record leasedMessage
	if json.Unmarshal(recordRaw, &record) != nil || !sameLease(record.Lease, lease) {
		return ErrLeaseLost
	}
	owner, err := q.Redis.Get(ctx, leaseMsgPrefix+record.MessageID).Result()
	if err != nil || owner != lease.LeaseID {
		return ErrLeaseLost
	}
	result, err := terminalScript.Run(ctx, q.Redis, []string{terminalKey, leaseIDPrefix + lease.LeaseID, leaseMsgPrefix + record.MessageID, StreamKey}, string(payload), lease.LeaseID, GroupName, int64(terminalTTL/time.Second), q.now().UnixMilli(), lease.ExpiresAt.UnixMilli(), record.MessageID).Int64()
	if err != nil {
		return err
	}
	switch result {
	case 0, 1:
		return nil
	case -1:
		return ErrLeaseLost
	case -2:
		return ErrTerminalConflict
	default:
		return ErrLeaseLost
	}
}

func sameLease(left, right contracts.WrapperLease) bool {
	return left.SchemaVersion == right.SchemaVersion && left.JobID == right.JobID && left.LeaseID == right.LeaseID && left.WorkerID == right.WorkerID && left.AccountID == right.AccountID && left.FenceEpoch == right.FenceEpoch && left.ExpiresAt.Equal(right.ExpiresAt)
}

func (q Queue) nextMessage(ctx context.Context, workerID string, leaseTTLSeconds int) (*redis.XMessage, error) {
	pending, err := q.Redis.XPendingExt(ctx, &redis.XPendingExtArgs{Stream: StreamKey, Group: GroupName, Start: "-", End: "+", Count: 32}).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	for _, item := range pending {
		exists, err := q.Redis.Exists(ctx, leaseMsgPrefix+item.ID).Result()
		if err != nil {
			return nil, err
		}
		if exists > 0 {
			continue
		}
		marker := "claiming:" + workerID
		acquired, err := q.Redis.SetNX(ctx, leaseMsgPrefix+item.ID, marker, time.Duration(leaseTTLSeconds)*time.Second).Result()
		if err != nil {
			return nil, err
		}
		if !acquired {
			continue
		}
		claimed, err := q.Redis.XClaim(ctx, &redis.XClaimArgs{Stream: StreamKey, Group: GroupName, Consumer: workerID, MinIdle: minClaimIdle, Messages: []string{item.ID}}).Result()
		if err != nil {
			if current, getErr := q.Redis.Get(ctx, leaseMsgPrefix+item.ID).Result(); getErr == nil && current == marker {
				_ = q.Redis.Del(ctx, leaseMsgPrefix+item.ID).Err()
			}
			return nil, err
		}
		if len(claimed) == 1 {
			return &claimed[0], nil
		}
		if current, getErr := q.Redis.Get(ctx, leaseMsgPrefix+item.ID).Result(); getErr == nil && current == marker {
			_ = q.Redis.Del(ctx, leaseMsgPrefix+item.ID).Err()
		}
	}
	streams, err := q.Redis.XReadGroup(ctx, &redis.XReadGroupArgs{Group: GroupName, Consumer: workerID, Streams: []string{StreamKey, ">"}, Count: 1, Block: time.Millisecond}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	if len(streams) == 0 || len(streams[0].Messages) == 0 {
		return nil, nil
	}
	message := streams[0].Messages[0]
	acquired, err := q.Redis.SetNX(ctx, leaseMsgPrefix+message.ID, "claiming:"+workerID, time.Duration(leaseTTLSeconds)*time.Second).Result()
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, nil
	}
	return &message, nil
}

func (q Queue) ensureGroup(ctx context.Context) error {
	err := q.Redis.XGroupCreateMkStream(ctx, StreamKey, GroupName, "0").Err()
	if err != nil && !strings.HasPrefix(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

func (q Queue) deadLetter(ctx context.Context, message redis.XMessage, reason error) error {
	if _, err := q.Redis.XAdd(ctx, &redis.XAddArgs{Stream: DLQKey, Values: map[string]any{
		"original_id": message.ID,
		"reason":      reason.Error(),
		"failed_at":   q.now().UTC().Format(time.RFC3339Nano),
	}}).Result(); err != nil {
		return err
	}
	return q.Redis.XAck(ctx, StreamKey, GroupName, message.ID).Err()
}

func (q Queue) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}

func decodeJob(message redis.XMessage) (contracts.WrapperJob, error) {
	raw, ok := message.Values["payload"]
	if !ok {
		return contracts.WrapperJob{}, fmt.Errorf("%w: wrapper stream payload is missing", contracts.ErrInvalidContract)
	}
	var job contracts.WrapperJob
	if err := json.Unmarshal([]byte(fmt.Sprint(raw)), &job); err != nil {
		return contracts.WrapperJob{}, fmt.Errorf("decode wrapper job: %w", err)
	}
	if err := job.Validate(); err != nil {
		return contracts.WrapperJob{}, err
	}
	if streamJobID := fmt.Sprint(message.Values["job_id"]); streamJobID != job.JobID {
		return contracts.WrapperJob{}, fmt.Errorf("%w: wrapper stream job identity differs", contracts.ErrInvalidContract)
	}
	return job, nil
}

func newLeaseID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
