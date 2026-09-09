package wrapper

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

type processRedisConfig struct {
	addr            string
	adminUser       string
	adminPassword   string
	controlUser     string
	controlPassword string
	wrapperUser     string
	wrapperPassword string
	gatewayUser     string
	gatewayPassword string
}

func processRedisFromEnv(t *testing.T) processRedisConfig {
	t.Helper()
	cfg := processRedisConfig{
		addr:            os.Getenv("GATEWAY_TEST_REDIS_ADDR"),
		adminUser:       os.Getenv("GATEWAY_TEST_REDIS_ADMIN_USERNAME"),
		adminPassword:   os.Getenv("GATEWAY_TEST_REDIS_ADMIN_PASSWORD"),
		controlUser:     os.Getenv("GATEWAY_TEST_REDIS_CONTROL_USERNAME"),
		controlPassword: os.Getenv("GATEWAY_TEST_REDIS_CONTROL_PASSWORD"),
		wrapperUser:     os.Getenv("GATEWAY_TEST_REDIS_WRAPPER_USERNAME"),
		wrapperPassword: os.Getenv("GATEWAY_TEST_REDIS_WRAPPER_PASSWORD"),
		gatewayUser:     os.Getenv("GATEWAY_TEST_REDIS_GATEWAY_USERNAME"),
		gatewayPassword: os.Getenv("GATEWAY_TEST_REDIS_GATEWAY_PASSWORD"),
	}
	if cfg.addr == "" || cfg.adminUser == "" || cfg.adminPassword == "" || cfg.controlUser == "" || cfg.controlPassword == "" || cfg.wrapperUser == "" || cfg.wrapperPassword == "" || cfg.gatewayUser == "" || cfg.gatewayPassword == "" {
		t.Skip("real Redis 7 wrapper process acceptance environment is not configured")
	}
	return cfg
}

func processRedisClient(cfg processRedisConfig, username, password string) *redis.Client {
	return redis.NewClient(&redis.Options{Addr: cfg.addr, Username: username, Password: password})
}

func TestP2RealRedisWrapperProcessLifecycle(t *testing.T) {
	cfg := processRedisFromEnv(t)
	ctx := context.Background()
	admin := processRedisClient(cfg, cfg.adminUser, cfg.adminPassword)
	controlRedis := processRedisClient(cfg, cfg.controlUser, cfg.controlPassword)
	wrapperRedis := processRedisClient(cfg, cfg.wrapperUser, cfg.wrapperPassword)
	gatewayRedis := processRedisClient(cfg, cfg.gatewayUser, cfg.gatewayPassword)
	defer admin.Close()
	defer controlRedis.Close()
	defer wrapperRedis.Close()
	defer gatewayRedis.Close()
	if err := admin.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	info, err := admin.Info(ctx, "server").Result()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(info, "redis_version:7.0.15") {
		t.Fatalf("wrapper process acceptance requires Redis 7.0.15: %s", info)
	}
	wrapperBinary := buildWrapperBinary(t)

	controlQueue := Queue{Redis: controlRedis}
	wrapperQueue := Queue{Redis: wrapperRedis}
	fixtureJob := contracts.WrapperJob{
		SchemaVersion:  contracts.SchemaVersion,
		JobID:          "process-fixture-job",
		Provider:       "codex",
		Operation:      contracts.WrapperRefresh,
		AccountID:      "account-process",
		FenceEpoch:     9,
		EncryptedInput: []byte("encrypted-fixture-envelope"),
	}
	if _, err := controlQueue.Enqueue(ctx, fixtureJob); err != nil {
		t.Fatalf("control enqueue: %v", err)
	}
	worker := startWrapperProcess(t, wrapperBinary, cfg, "process-worker", 30, 0)
	waitForTerminal(t, wrapperQueue, fixtureJob.JobID)
	stopWrapperProcess(t, worker)

	// A second completion against the same terminal is accepted on real Redis.
	idempotentJob := contracts.WrapperJob{SchemaVersion: contracts.SchemaVersion, JobID: "process-idempotent-job", Provider: "claude", Operation: contracts.WrapperAuthorize, EncryptedInput: []byte("encrypted-idempotent-envelope")}
	if _, err := controlQueue.Enqueue(ctx, idempotentJob); err != nil {
		t.Fatal(err)
	}
	_, lease, err := wrapperQueue.Claim(ctx, contracts.WrapperClaim{SchemaVersion: contracts.SchemaVersion, WorkerID: "idempotent-worker", LeaseTTLSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	completion := contracts.WrapperCompletion{SchemaVersion: contracts.SchemaVersion, JobID: idempotentJob.JobID, LeaseID: lease.LeaseID, WorkerID: lease.WorkerID, EncryptedOutput: []byte("encrypted-idempotent-output")}
	if err := wrapperQueue.Complete(ctx, lease, completion); err != nil {
		t.Fatal(err)
	}
	if err := wrapperQueue.Complete(ctx, lease, completion); err != nil {
		t.Fatalf("duplicate completion: %v", err)
	}

	crashJob := contracts.WrapperJob{SchemaVersion: contracts.SchemaVersion, JobID: "process-crash-job", Provider: "codex", Operation: contracts.WrapperRefresh, AccountID: "account-crash", FenceEpoch: 10, EncryptedInput: []byte("encrypted-crash-envelope")}
	if _, err := controlQueue.Enqueue(ctx, crashJob); err != nil {
		t.Fatal(err)
	}
	crashed := startWrapperProcess(t, wrapperBinary, cfg, "crash-worker", 1, 5000)
	waitForPending(t, wrapperRedis)
	if err := crashed.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := crashed.Wait(); err == nil {
		t.Fatal("SIGKILL helper exited normally")
	}
	time.Sleep(1200 * time.Millisecond)
	reclaimer := startWrapperProcess(t, wrapperBinary, cfg, "reclaimer-worker", 30, 0)
	waitForTerminal(t, wrapperQueue, crashJob.JobID)
	stopWrapperProcess(t, reclaimer)

	oldJob := contracts.WrapperJob{SchemaVersion: contracts.SchemaVersion, JobID: "process-fenced-job", Provider: "codex", Operation: contracts.WrapperRefresh, AccountID: "account-fenced", FenceEpoch: 11, EncryptedInput: []byte("encrypted-fenced-envelope")}
	if _, err := controlQueue.Enqueue(ctx, oldJob); err != nil {
		t.Fatal(err)
	}
	_, oldLease, err := wrapperQueue.Claim(ctx, contracts.WrapperClaim{SchemaVersion: contracts.SchemaVersion, WorkerID: "old-worker", LeaseTTLSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	_, currentLease, err := wrapperQueue.Claim(ctx, contracts.WrapperClaim{SchemaVersion: contracts.SchemaVersion, WorkerID: "current-worker", LeaseTTLSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	stale := contracts.WrapperCompletion{SchemaVersion: contracts.SchemaVersion, JobID: oldJob.JobID, LeaseID: oldLease.LeaseID, WorkerID: oldLease.WorkerID, AccountID: oldLease.AccountID, FenceEpoch: oldLease.FenceEpoch, EncryptedOutput: []byte("stale-output")}
	if err := wrapperQueue.Complete(ctx, oldLease, stale); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old worker completion: %v", err)
	}
	current := contracts.WrapperCompletion{SchemaVersion: contracts.SchemaVersion, JobID: oldJob.JobID, LeaseID: currentLease.LeaseID, WorkerID: currentLease.WorkerID, AccountID: currentLease.AccountID, FenceEpoch: currentLease.FenceEpoch, EncryptedOutput: []byte("current-output")}
	if err := wrapperQueue.Complete(ctx, currentLease, current); err != nil {
		t.Fatal(err)
	}

	aclJob := contracts.WrapperJob{SchemaVersion: contracts.SchemaVersion, JobID: "process-acl-job", Provider: "codex", Operation: contracts.WrapperRefresh, AccountID: "account-acl", FenceEpoch: 12, EncryptedInput: []byte("encrypted-acl-envelope")}
	if _, err := controlQueue.Enqueue(ctx, aclJob); err != nil {
		t.Fatal(err)
	}
	_, aclLease, err := wrapperQueue.Claim(ctx, contracts.WrapperClaim{SchemaVersion: contracts.SchemaVersion, WorkerID: "acl-probe-worker", LeaseTTLSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	assertNoWrapperRead(t, gatewayRedis, StreamKey)
	assertNoWrapperRead(t, gatewayRedis, leaseIDPrefix+aclLease.LeaseID)
	assertNoWrapperRead(t, gatewayRedis, TerminalPrefix+fixtureJob.JobID)
}

func buildWrapperBinary(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate wrapper integration test")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "../../.."))
	goBinary := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(goBinary); err != nil {
		t.Skipf("go toolchain is unavailable for CLI process acceptance: %v", err)
	}
	binary := filepath.Join(t.TempDir(), "gwd")
	cmd := exec.Command(goBinary, "build", "-buildvcs=false", "-o", binary, "./cmd/gwd")
	cmd.Dir = repoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build gwd wrapper process: %v\n%s", err, output)
	}
	return binary
}

func startWrapperProcess(t *testing.T, binary string, cfg processRedisConfig, workerID string, leaseTTL, fixtureDelayMS int) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(binary, "wrapper", "run")
	cmd.Env = append(os.Environ(),
		"GATEWAY_WRAPPER_REDIS_ADDR="+cfg.addr,
		"GATEWAY_WRAPPER_REDIS_USERNAME="+cfg.wrapperUser,
		"GATEWAY_WRAPPER_REDIS_PASSWORD="+cfg.wrapperPassword,
		"GATEWAY_WRAPPER_WORKER_ID="+workerID,
		"GATEWAY_WRAPPER_LEASE_TTL_SECONDS="+itoa(leaseTTL),
		"GATEWAY_WRAPPER_POLL_INTERVAL_MS=10",
		"GATEWAY_WRAPPER_EXECUTOR=fixture",
		"GATEWAY_WRAPPER_FIXTURE_DELAY_MS="+itoa(fixtureDelayMS),
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func stopWrapperProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wrapper graceful stop: %v", err)
	}
}

func waitForTerminal(t *testing.T, queue Queue, jobID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := queue.Terminal(context.Background(), jobID); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("terminal was not written for %s", jobID)
}

func waitForPending(t *testing.T, rdb redis.UniversalClient) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := rdb.XPending(context.Background(), StreamKey, GroupName).Result()
		if err == nil && pending.Count > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("wrapper process did not claim the crash job")
}

func assertNoWrapperRead(t *testing.T, gateway redis.UniversalClient, key string) {
	t.Helper()
	if _, err := gateway.Get(context.Background(), key).Result(); err == nil || !redisACLDenied(err) {
		t.Fatalf("gateway read %s: %v", key, err)
	}
}

func itoa(value int) string {
	return strconv.Itoa(value)
}
