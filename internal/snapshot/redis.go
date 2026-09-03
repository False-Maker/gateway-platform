package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

const (
	GraceTTL = 60 * time.Second
	DataTTL  = 120 * time.Second
)

var ErrStaleEpoch = errors.New("stale snapshot epoch")

func BucketID(platform, group string) string {
	h := sha256.Sum256([]byte(platform + "\x00" + group))
	return hex.EncodeToString(h[:])[:16]
}

func Keys(bucket string) (set, active, version, epoch, retired, lock string) {
	return "snap:buckets", "snap:active:" + bucket, "snap:ver:" + bucket,
		"snap:epoch:" + bucket, "snap:retired:" + bucket, "snap:lock:" + bucket
}

// Entry wraps an account with enough envelope data for a gateway to reject a
// mixed or incomplete version. Version is omitted by the publisher because the
// Redis script allocates it atomically; the active key remains authoritative.
type Entry struct {
	Platform string            `json:"platform"`
	Group    string            `json:"group"`
	BucketID string            `json:"bucket_id"`
	Version  int64             `json:"version,omitempty"`
	Epoch    int64             `json:"epoch"`
	Account  contracts.Account `json:"account"`
}

type Publisher struct{ Redis redis.UniversalClient }

var publishScript = redis.NewScript(`
local current = redis.call('GET', KEYS[4])
if (current and tonumber(current) ~= tonumber(ARGV[1])) or (not current and tonumber(ARGV[1]) ~= 0) then return {err='STALE_EPOCH'} end
local old = redis.call('GET', KEYS[2])
local version = redis.call('INCR', KEYS[3])
local data = 'snap:data:' .. ARGV[6] .. ':' .. version
redis.call('DEL', data)
redis.call('HSET', data, '__version', version)
for i = 7, #ARGV, 2 do
  redis.call('HSET', data, ARGV[i], ARGV[i+1])
end
redis.call('EXPIRE', data, ARGV[3])
redis.call('SET', KEYS[2], version)
redis.call('SADD', KEYS[1], ARGV[6])
redis.call('SET', KEYS[4], ARGV[2])
if old then
  redis.call('ZADD', KEYS[5], ARGV[5], old)
  redis.call('EXPIRE', KEYS[5], ARGV[4])
  redis.call('EXPIRE', 'snap:data:' .. ARGV[6] .. ':' .. old, ARGV[4])
end
return version
`)

// Publish atomically flips the active version after checking the bucket epoch.
func (p Publisher) Publish(ctx context.Context, platform, group string, expectedEpoch int64, accounts []contracts.Account) (int64, error) {
	if p.Redis == nil {
		return 0, errors.New("redis client is nil")
	}
	if expectedEpoch < 0 {
		return 0, fmt.Errorf("expected snapshot epoch must be non-negative")
	}
	bucket := BucketID(platform, group)
	_, active, versionKey, epochKey, retired, lock := Keys(bucket)
	nextEpoch := expectedEpoch + 1
	args := []any{expectedEpoch, nextEpoch, int(DataTTL / time.Second), int(GraceTTL / time.Second), time.Now().Add(GraceTTL).UnixMilli(), bucket}
	for _, account := range accounts {
		if account.Platform != platform || account.Group != group {
			return 0, fmt.Errorf("account %s does not belong to snapshot bucket", account.ID)
		}
		entry := Entry{Platform: platform, Group: group, BucketID: bucket, Epoch: nextEpoch, Account: account}
		value, err := json.Marshal(entry)
		if err != nil {
			return 0, err
		}
		args = append(args, account.ID, string(value))
	}
	result, err := publishScript.Run(ctx, p.Redis, []string{"snap:buckets", active, versionKey, epochKey, retired, lock}, args...).Result()
	if err != nil {
		if strings.Contains(err.Error(), "STALE_EPOCH") {
			return 0, ErrStaleEpoch
		}
		return 0, err
	}
	version, err := strconv.ParseInt(fmt.Sprint(result), 10, 64)
	if err != nil {
		return 0, err
	}
	return version, nil
}

type Loader struct{ Redis redis.UniversalClient }

type Snapshot struct {
	Platform string
	Group    string
	BucketID string
	Version  int64
	Epoch    int64
	// ValidUntil is derived from the active Redis data key TTL. It is local
	// loader metadata, not part of the cross-process snapshot JSON contract.
	ValidUntil time.Time
	Accounts   []contracts.Account
}

// Store keeps the last known-good snapshot. A failed refresh never replaces it,
// so a transient Redis/version error cannot load a partial bucket into gateway.
type Store struct {
	Loader Loader
	mu     sync.RWMutex
	items  map[string]Snapshot
}

func NewStore(loader Loader) *Store { return &Store{Loader: loader, items: make(map[string]Snapshot)} }

func (s *Store) Refresh(ctx context.Context, platform, group string) error {
	next, err := s.Loader.Load(ctx, platform, group)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.items[platform+"\x00"+group] = next
	s.mu.Unlock()
	return nil
}

func (s *Store) Current(platform, group string) (Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	next, ok := s.items[platform+"\x00"+group]
	return next, ok
}

// CurrentFresh returns the current snapshot only while the Redis version that
// supplied it is still valid. A refresh failure may keep last-known-good data
// in memory, but gateway must not schedule from it after this point.
func (s *Store) CurrentFresh(platform, group string) (Snapshot, bool) {
	return s.CurrentFreshAt(platform, group, time.Now())
}

func (s *Store) CurrentFreshAt(platform, group string, now time.Time) (Snapshot, bool) {
	current, ok := s.Current(platform, group)
	if !ok || current.ValidUntil.IsZero() || !current.ValidUntil.After(now) {
		return Snapshot{}, false
	}
	return current, true
}

func (l Loader) Load(ctx context.Context, platform, group string) (Snapshot, error) {
	if l.Redis == nil {
		return Snapshot{}, errors.New("redis client is nil")
	}
	bucket := BucketID(platform, group)
	_, activeKey, _, epochKey, _, _ := Keys(bucket)
	active, err := l.Redis.Get(ctx, activeKey).Result()
	if err != nil {
		return Snapshot{}, fmt.Errorf("load active snapshot: %w", err)
	}
	version, err := strconv.ParseInt(active, 10, 64)
	if err != nil || version <= 0 {
		return Snapshot{}, fmt.Errorf("invalid active snapshot version %q", active)
	}
	epochValue, err := l.Redis.Get(ctx, epochKey).Result()
	if err != nil {
		return Snapshot{}, fmt.Errorf("load snapshot epoch: %w", err)
	}
	epoch, err := strconv.ParseInt(epochValue, 10, 64)
	if err != nil {
		return Snapshot{}, fmt.Errorf("invalid snapshot epoch %q", epochValue)
	}
	dataKey := fmt.Sprintf("snap:data:%s:%d", bucket, version)
	values, err := l.Redis.HGetAll(ctx, dataKey).Result()
	if err != nil {
		return Snapshot{}, fmt.Errorf("load snapshot data: %w", err)
	}
	dataTTL, err := l.Redis.TTL(ctx, dataKey).Result()
	if err != nil {
		return Snapshot{}, fmt.Errorf("load snapshot data TTL: %w", err)
	}
	if dataTTL <= 0 {
		return Snapshot{}, fmt.Errorf("snapshot data expired for bucket %s", bucket)
	}
	if dataVersion, ok := values["__version"]; !ok || dataVersion != strconv.FormatInt(version, 10) {
		return Snapshot{}, fmt.Errorf("snapshot data version mismatch for bucket %s", bucket)
	}
	delete(values, "__version")
	accounts := make([]contracts.Account, 0, len(values))
	for accountID, raw := range values {
		var entry Entry
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			return Snapshot{}, fmt.Errorf("decode account %s: %w", accountID, err)
		}
		if entry.BucketID != bucket || (entry.Version != 0 && entry.Version != version) || entry.Epoch != epoch || entry.Platform != platform || entry.Group != group || entry.Account.ID != accountID {
			return Snapshot{}, fmt.Errorf("snapshot envelope mismatch for account %s", accountID)
		}
		accounts = append(accounts, entry.Account)
	}
	return Snapshot{Platform: platform, Group: group, BucketID: bucket, Version: version, Epoch: epoch, ValidUntil: time.Now().Add(dataTTL), Accounts: accounts}, nil
}
