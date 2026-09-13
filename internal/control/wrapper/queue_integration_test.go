package wrapper

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

// reclaimDeadline bounds how long these tests wait for a lease taken with
// LeaseTTLSeconds=1 to become reclaimable by another worker.
//
// Do not replace the polling below with `time.Sleep(something just over 1s)`.
// That is what these tests used to do, and it made them flaky: Queue.nextMessage
// reclaims through XCLAIM with MinIdle=minClaimIdle (1s), and a stream entry's
// idle time is computed by Redis as `server_now - last_delivery`, entirely from
// the *server's* clock. Under load the test Redis container's clock steps
// backward, which shrinks idle by the size of the step and makes XCLAIM refuse a
// lease that has really expired; nextMessage then falls through to XREADGROUP,
// finds nothing new, and Claim returns redis.Nil.
//
// Two instrumented observations, both server-side:
//   - 2 of 2204 MONITOR timestamps went backward by ~435ms, and each of those
//     two steps produced exactly one failure of the queue test.
//   - after a 2s sleep the pending entry still reported idle=866ms and the 1s
//     lease keys had not expired -- a step of roughly 1.13s.
//
// The steps have no useful upper bound, so no fixed margin is safe. Polling is:
// it asserts the same property ("an expired lease becomes reclaimable, under a
// new lease id") without encoding how long the environment takes to get there.
// This is an environment property (Docker Desktop on Windows), not a defect in
// the queue: see docs/EVIDENCE.md, "wrapper reclaim 偶发失败的根因".
const reclaimDeadline = 30 * time.Second

// claimReclaimedLease polls until the previous lease has been handed to another
// worker, and fails the test if that never happens.
func claimReclaimedLease(t *testing.T, ctx context.Context, queue Queue, workerID string, previous contracts.WrapperLease) contracts.WrapperLease {
	t.Helper()
	deadline := time.Now().Add(reclaimDeadline)
	var lastErr error
	for time.Now().Before(deadline) {
		_, lease, err := queue.Claim(ctx, contracts.WrapperClaim{SchemaVersion: contracts.SchemaVersion, WorkerID: workerID, LeaseTTLSeconds: 30})
		if err == nil && lease.LeaseID != previous.LeaseID {
			return lease
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("lease %s was never reclaimed within %s (last claim error %v)", previous.LeaseID, reclaimDeadline, lastErr)
	return contracts.WrapperLease{}
}

func TestP2RealRedisQueueLifecycleAndGatewayIsolation(t *testing.T) {
	addr := os.Getenv("GATEWAY_TEST_REDIS_ADDR")
	adminUser := os.Getenv("GATEWAY_TEST_REDIS_ADMIN_USERNAME")
	adminPassword := os.Getenv("GATEWAY_TEST_REDIS_ADMIN_PASSWORD")
	queueUser := os.Getenv("GATEWAY_TEST_REDIS_WRAPPER_USERNAME")
	queuePassword := os.Getenv("GATEWAY_TEST_REDIS_WRAPPER_PASSWORD")
	gatewayUser := os.Getenv("GATEWAY_TEST_REDIS_GATEWAY_USERNAME")
	gatewayPassword := os.Getenv("GATEWAY_TEST_REDIS_GATEWAY_PASSWORD")
	if addr == "" || adminUser == "" || adminPassword == "" || queueUser == "" || queuePassword == "" || gatewayUser == "" || gatewayPassword == "" {
		t.Skip("real Redis ACL acceptance environment is not configured")
	}
	ctx := context.Background()
	admin := redis.NewClient(&redis.Options{Addr: addr, Username: adminUser, Password: adminPassword})
	queueRedis := redis.NewClient(&redis.Options{Addr: addr, Username: queueUser, Password: queuePassword})
	gateway := redis.NewClient(&redis.Options{Addr: addr, Username: gatewayUser, Password: gatewayPassword})
	defer admin.Close()
	defer queueRedis.Close()
	defer gateway.Close()
	if err := admin.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}

	queue := Queue{Redis: queueRedis}
	job := contracts.WrapperJob{
		SchemaVersion:  contracts.SchemaVersion,
		JobID:          "real-redis-job",
		Provider:       "codex",
		Operation:      contracts.WrapperRefresh,
		AccountID:      "account-real-redis",
		FenceEpoch:     7,
		EncryptedInput: []byte("opaque-ciphertext"),
	}
	firstID, err := queue.Enqueue(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := queue.Enqueue(ctx, job)
	if err != nil || firstID != secondID {
		t.Fatalf("idempotent enqueue: first=%q second=%q err=%v", firstID, secondID, err)
	}
	if _, err := gateway.XRange(ctx, StreamKey, "-", "+").Result(); err == nil || !redisACLDenied(err) {
		t.Fatalf("gateway unexpectedly read wrapper jobs: %v", err)
	}

	_, oldLease, err := queue.Claim(ctx, contracts.WrapperClaim{SchemaVersion: contracts.SchemaVersion, WorkerID: "worker-old", LeaseTTLSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	currentLease := claimReclaimedLease(t, ctx, queue, "worker-current", oldLease)
	stale := contracts.WrapperCompletion{
		SchemaVersion:   contracts.SchemaVersion,
		JobID:           job.JobID,
		LeaseID:         oldLease.LeaseID,
		WorkerID:        oldLease.WorkerID,
		AccountID:       job.AccountID,
		FenceEpoch:      job.FenceEpoch,
		EncryptedOutput: []byte("stale-output"),
	}
	if err := queue.Complete(ctx, oldLease, stale); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale worker completion: %v", err)
	}
	completion := contracts.WrapperCompletion{
		SchemaVersion:   contracts.SchemaVersion,
		JobID:           job.JobID,
		LeaseID:         currentLease.LeaseID,
		WorkerID:        currentLease.WorkerID,
		AccountID:       job.AccountID,
		FenceEpoch:      job.FenceEpoch,
		EncryptedOutput: []byte("current-output"),
	}
	if err := queue.Complete(ctx, currentLease, completion); err != nil {
		t.Fatal(err)
	}
	if err := queue.Complete(ctx, currentLease, completion); err != nil {
		t.Fatalf("idempotent completion: %v", err)
	}
	if _, err := gateway.Get(ctx, TerminalPrefix+job.JobID).Result(); err == nil || !redisACLDenied(err) {
		t.Fatalf("gateway unexpectedly read wrapper terminal result: %v", err)
	}
	if pending := queueRedis.XPending(ctx, StreamKey, GroupName).Val().Count; pending != 0 {
		t.Fatalf("pending jobs after completion=%d", pending)
	}
}

func redisACLDenied(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToUpper(err.Error())
	return strings.Contains(message, "USER EXECUTING THE SCRIPT CAN'T RUN THIS COMMAND") || strings.Contains(message, "NOPERM")
}
