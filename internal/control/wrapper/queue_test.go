package wrapper

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

func TestQueueClaimCompleteAndTerminalAreIdempotent(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	now := time.Now().UTC()
	queue := Queue{Redis: rdb, Now: func() time.Time { return now }}
	job := contracts.WrapperJob{SchemaVersion: contracts.SchemaVersion, JobID: "job-1", Provider: "codex", Operation: contracts.WrapperRefresh, AccountID: "account-1", FenceEpoch: 7, EncryptedInput: []byte("encrypted-input")}
	firstID, err := queue.Enqueue(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := queue.Enqueue(context.Background(), job)
	if err != nil || firstID != secondID || rdb.XLen(context.Background(), StreamKey).Val() != 1 {
		t.Fatalf("duplicate enqueue: first=%q second=%q len=%d err=%v", firstID, secondID, rdb.XLen(context.Background(), StreamKey).Val(), err)
	}
	claimed, lease, err := queue.Claim(context.Background(), contracts.WrapperClaim{SchemaVersion: contracts.SchemaVersion, WorkerID: "worker-1", LeaseTTLSeconds: 60})
	if err != nil || claimed.JobID != job.JobID || lease.WorkerID != "worker-1" {
		t.Fatalf("job=%#v lease=%#v err=%v", claimed, lease, err)
	}
	completion := contracts.WrapperCompletion{SchemaVersion: contracts.SchemaVersion, JobID: job.JobID, LeaseID: lease.LeaseID, WorkerID: lease.WorkerID, AccountID: job.AccountID, FenceEpoch: job.FenceEpoch, EncryptedOutput: []byte("encrypted-output")}
	if err := queue.Complete(context.Background(), lease, completion); err != nil {
		t.Fatal(err)
	}
	if err := queue.Complete(context.Background(), lease, completion); err != nil {
		t.Fatalf("idempotent completion failed: %v", err)
	}
	now = lease.ExpiresAt.Add(time.Second)
	if err := queue.Complete(context.Background(), lease, completion); err != nil {
		t.Fatalf("post-expiry idempotent completion failed: %v", err)
	}
	terminal, err := queue.Terminal(context.Background(), job.JobID)
	if err != nil || len(terminal) == 0 {
		t.Fatalf("terminal=%q err=%v", terminal, err)
	}
	if pending := rdb.XPending(context.Background(), StreamKey, GroupName).Val().Count; pending != 0 {
		t.Fatalf("pending=%d", pending)
	}
}

func TestQueueReclaimsExpiredLeaseAndRejectsOldWorker(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	now := time.Now().UTC()
	queue := Queue{Redis: rdb, Now: func() time.Time { return now }}
	job := contracts.WrapperJob{SchemaVersion: contracts.SchemaVersion, JobID: "job-2", Provider: "claude", Operation: contracts.WrapperRefresh, AccountID: "account-2", FenceEpoch: 3, EncryptedInput: []byte("encrypted-input")}
	if _, err := queue.Enqueue(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	_, oldLease, err := queue.Claim(context.Background(), contracts.WrapperClaim{SchemaVersion: contracts.SchemaVersion, WorkerID: "worker-old", LeaseTTLSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	mini.FastForward(2 * time.Second)
	now = now.Add(2 * time.Second)
	_, newLease, err := queue.Claim(context.Background(), contracts.WrapperClaim{SchemaVersion: contracts.SchemaVersion, WorkerID: "worker-new", LeaseTTLSeconds: 60})
	if err != nil || newLease.LeaseID == oldLease.LeaseID {
		t.Fatalf("new lease=%#v old=%#v err=%v", newLease, oldLease, err)
	}
	oldCompletion := contracts.WrapperCompletion{SchemaVersion: contracts.SchemaVersion, JobID: job.JobID, LeaseID: oldLease.LeaseID, WorkerID: oldLease.WorkerID, AccountID: job.AccountID, FenceEpoch: job.FenceEpoch, EncryptedOutput: []byte("old-output")}
	if err := queue.Complete(context.Background(), oldLease, oldCompletion); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old completion got %v, want lease lost", err)
	}
	failure := contracts.WrapperFailure{SchemaVersion: contracts.SchemaVersion, JobID: job.JobID, LeaseID: newLease.LeaseID, WorkerID: newLease.WorkerID, AccountID: job.AccountID, FenceEpoch: job.FenceEpoch, Code: "oauth_failed", Retryable: true}
	if err := queue.Fail(context.Background(), newLease, failure); err != nil {
		t.Fatal(err)
	}
}

func TestQueueDeadLettersInvalidJobWithoutBlockingNextClaim(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	queue := Queue{Redis: rdb, Now: time.Now}
	if err := queue.ensureGroup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := rdb.XAdd(context.Background(), &redis.XAddArgs{Stream: StreamKey, Values: map[string]any{"job_id": "bad", "payload": `{}`}}).Result(); err != nil {
		t.Fatal(err)
	}
	job := contracts.WrapperJob{SchemaVersion: contracts.SchemaVersion, JobID: "job-good", Provider: "codex", Operation: contracts.WrapperAuthorize, EncryptedInput: []byte("encrypted")}
	if _, err := queue.Enqueue(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := queue.Claim(context.Background(), contracts.WrapperClaim{SchemaVersion: contracts.SchemaVersion, WorkerID: "worker-1", LeaseTTLSeconds: 60})
	if err != nil || claimed.JobID != job.JobID || rdb.XLen(context.Background(), DLQKey).Val() != 1 {
		t.Fatalf("claimed=%#v dlq=%d err=%v", claimed, rdb.XLen(context.Background(), DLQKey).Val(), err)
	}
}

func TestQueueConcurrentClaimsAreSingleOwner(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	queue := Queue{Redis: rdb}
	job := contracts.WrapperJob{SchemaVersion: contracts.SchemaVersion, JobID: "concurrent-job", Provider: "codex", Operation: contracts.WrapperAuthorize, EncryptedInput: []byte("opaque")}
	if _, err := queue.Enqueue(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	claims := 0
	for i := 0; i < 32; i++ {
		workerID := fmt.Sprintf("worker-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := queue.Claim(context.Background(), contracts.WrapperClaim{SchemaVersion: contracts.SchemaVersion, WorkerID: workerID, LeaseTTLSeconds: 60})
			if err == nil {
				mu.Lock()
				claims++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if claims != 1 {
		t.Fatalf("successful claims=%d, want exactly one", claims)
	}
}
