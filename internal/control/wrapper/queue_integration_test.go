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
	time.Sleep(1100 * time.Millisecond)
	_, currentLease, err := queue.Claim(ctx, contracts.WrapperClaim{SchemaVersion: contracts.SchemaVersion, WorkerID: "worker-current", LeaseTTLSeconds: 30})
	if err != nil || currentLease.LeaseID == oldLease.LeaseID {
		t.Fatalf("reclaimed lease=%#v old=%#v err=%v", currentLease, oldLease, err)
	}
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
