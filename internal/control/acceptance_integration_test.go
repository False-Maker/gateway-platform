package control_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/elucid/gateway-platform/internal/control"
	"github.com/elucid/gateway-platform/internal/events"
	"github.com/elucid/gateway-platform/internal/gateway"
	"github.com/elucid/gateway-platform/internal/snapshot"
	"github.com/elucid/gateway-platform/migrations"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const (
	crashModeEnv     = "GATEWAY_ACCEPTANCE_CRASH_MODE"
	controlCrashMode = "control_after_commit"
	gatewayCrashMode = "gateway_after_attempt_started"
)

type acceptanceRedis struct {
	addr            string
	adminUser       string
	adminPassword   string
	gatewayUser     string
	gatewayPassword string
	controlUser     string
	controlPassword string
}

func acceptanceRedisConfig(t *testing.T) acceptanceRedis {
	t.Helper()
	cfg := acceptanceRedis{
		addr:            os.Getenv("GATEWAY_TEST_REDIS_ADDR"),
		adminUser:       os.Getenv("GATEWAY_TEST_REDIS_ADMIN_USERNAME"),
		adminPassword:   os.Getenv("GATEWAY_TEST_REDIS_ADMIN_PASSWORD"),
		gatewayUser:     os.Getenv("GATEWAY_TEST_REDIS_GATEWAY_USERNAME"),
		gatewayPassword: os.Getenv("GATEWAY_TEST_REDIS_GATEWAY_PASSWORD"),
		controlUser:     os.Getenv("GATEWAY_TEST_REDIS_CONTROL_USERNAME"),
		controlPassword: os.Getenv("GATEWAY_TEST_REDIS_CONTROL_PASSWORD"),
	}
	if cfg.addr == "" || cfg.adminUser == "" || cfg.adminPassword == "" || cfg.gatewayUser == "" || cfg.gatewayPassword == "" || cfg.controlUser == "" || cfg.controlPassword == "" {
		t.Skip("real Redis ACL acceptance environment is not configured")
	}
	return cfg
}

func redisClient(addr, username, password string) *redis.Client {
	return redis.NewClient(&redis.Options{Addr: addr, Username: username, Password: password})
}

type acceptanceHandler struct {
	attempts  int
	failEvery bool
}

func (h *acceptanceHandler) HandleAttemptStarted(context.Context, contracts.AttemptStarted) error {
	h.attempts++
	if h.failEvery {
		return errors.New("injected persistent failure")
	}
	return nil
}

func (h *acceptanceHandler) HandleRelease(context.Context, contracts.Release) error  { return nil }
func (h *acceptanceHandler) RecoverAttempts(context.Context, time.Time) (int, error) { return 0, nil }

func acceptanceAttempt(now time.Time, suffix string) contracts.AttemptStarted {
	return contracts.AttemptStarted{
		SchemaVersion: contracts.SchemaVersion,
		EventID:       "start-" + suffix,
		RequestID:     "request-" + suffix,
		AttemptID:     "attempt-" + suffix,
		AttemptNo:     1,
		ProducerID:    "gateway-acceptance",
		OccurredAt:    now,
		AccountID:     "account-acceptance",
		Provider:      "apikey",
		Model:         "acceptance-model",
		TenantID:      "acceptance-tenant",
		DeadlineAt:    now.Add(time.Minute),
	}
}

func attemptEvent(t *testing.T, attempt contracts.AttemptStarted) contracts.StreamEvent {
	t.Helper()
	event, err := contracts.NewStreamEvent(contracts.EventTypeAttemptStarted, attempt.EventID, attempt.RequestID, attempt.AttemptID, attempt.ProducerID, attempt.OccurredAt, attempt)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func forcePendingIdle(t *testing.T, ctx context.Context, rdb redis.UniversalClient, messageID string) {
	t.Helper()
	if err := rdb.Do(ctx, "XCLAIM", events.StreamKey, events.GroupName, "stale-owner", 0, messageID, "IDLE", (events.ReclaimIdle + time.Second).Milliseconds(), "JUSTID").Err(); err != nil {
		t.Fatalf("force pending idle: %v", err)
	}
}

func TestAcceptanceRealRedisACLAndReliability(t *testing.T) {
	cfg := acceptanceRedisConfig(t)
	ctx := context.Background()
	admin := redisClient(cfg.addr, cfg.adminUser, cfg.adminPassword)
	gatewayRedis := redisClient(cfg.addr, cfg.gatewayUser, cfg.gatewayPassword)
	controlRedis := redisClient(cfg.addr, cfg.controlUser, cfg.controlPassword)
	defer admin.Close()
	defer gatewayRedis.Close()
	defer controlRedis.Close()
	if err := admin.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}

	account := contracts.Account{
		ID:       "account-acceptance",
		Provider: "apikey",
		Platform: "apikey",
		Group:    "default",
		Status:   "active",
		Credential: contracts.Credential{
			Kind:        "static",
			AccessToken: "acceptance-key",
			Version:     1,
		},
		Profile: contracts.UpstreamProfile{BaseURL: "http://127.0.0.1:1"},
		Limits:  contracts.AccountLimits{MaxConcurrency: 1, DegradePolicy: contracts.DegradeFailClosed},
	}
	publisher := snapshot.Publisher{Redis: controlRedis}
	if version, err := publisher.Publish(ctx, "apikey", "default", 0, []contracts.Account{account}); err != nil || version != 1 {
		t.Fatalf("control snapshot publish: version=%d err=%v", version, err)
	}
	if _, err := publisher.Publish(ctx, "apikey", "default", 1, []contracts.Account{account}); err != nil {
		t.Fatal(err)
	}
	oldDataKey := "snap:data:" + snapshot.BucketID("apikey", "default") + ":1"
	if ttl := admin.TTL(ctx, oldDataKey).Val(); ttl <= 0 || ttl > snapshot.GraceTTL {
		t.Fatalf("old snapshot grace TTL = %v", ttl)
	}
	loaded, err := (snapshot.Loader{Redis: gatewayRedis}).Load(ctx, "apikey", "default")
	if err != nil || loaded.Version != 2 || len(loaded.Accounts) != 1 {
		t.Fatalf("gateway snapshot load: snapshot=%#v err=%v", loaded, err)
	}
	if _, err := (snapshot.Publisher{Redis: gatewayRedis}).Publish(ctx, "apikey", "default", 2, []contracts.Account{account}); err == nil || !redisACLDenied(err) {
		t.Fatalf("gateway unexpectedly published a snapshot: %v", err)
	}
	if err := admin.Set(ctx, "credentials:refresh:account-acceptance", "secret", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := gatewayRedis.Get(ctx, "credentials:refresh:account-acceptance").Result(); err == nil || !redisACLDenied(err) {
		t.Fatalf("gateway unexpectedly read a refresh credential key: %v", err)
	}
	releaseLimit, err := (gateway.Limiter{Redis: gatewayRedis}).Acquire(ctx, contracts.Lease{AccountID: account.ID, Limits: account.Limits})
	if err != nil {
		t.Fatalf("gateway limiter Lua: %v", err)
	}
	releaseLimit()

	producer := events.Producer{Redis: gatewayRedis, ProducerID: "gateway-acceptance"}
	event := attemptEvent(t, acceptanceAttempt(time.Now().UTC(), "retry"))
	messageID, err := producer.Add(ctx, event)
	if err != nil {
		t.Fatalf("gateway stream append: %v", err)
	}
	handler := &acceptanceHandler{failEvery: true}
	consumer := events.Consumer{Redis: controlRedis, Consumer: "control-acceptance", Handler: handler, Now: time.Now}
	for delivery := 1; delivery <= events.MaxRetries; delivery++ {
		if delivery > 1 {
			forcePendingIdle(t, ctx, controlRedis, messageID)
		}
		if err := consumer.RunOnce(ctx); err == nil {
			t.Fatalf("delivery %d unexpectedly succeeded", delivery)
		}
	}
	forcePendingIdle(t, ctx, controlRedis, messageID)
	if err := consumer.RunOnce(ctx); err != nil {
		t.Fatalf("DLQ transition: %v", err)
	}
	if handler.attempts != events.MaxRetries {
		t.Fatalf("handler calls=%d want=%d", handler.attempts, events.MaxRetries)
	}
	if pending := controlRedis.XPending(ctx, events.StreamKey, events.GroupName).Val().Count; pending != 0 {
		t.Fatalf("pending after DLQ=%d", pending)
	}
	if dlqLength, err := admin.XLen(ctx, events.DLQKey).Result(); err != nil || dlqLength != 1 {
		t.Fatalf("DLQ length=%d err=%v", dlqLength, err)
	}

	if err := admin.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	oldID := fmt.Sprintf("%d-0", now.Add(-events.Retention-time.Hour).UnixMilli())
	freshID := fmt.Sprintf("%d-0", now.Add(-time.Hour).UnixMilli())
	for _, id := range []string{oldID, freshID} {
		if err := admin.XAdd(ctx, &redis.XAddArgs{Stream: events.StreamKey, ID: id, Values: map[string]any{"probe": id}}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	if err := controlRedis.XGroupCreateMkStream(ctx, events.StreamKey, events.GroupName, "0").Err(); err != nil {
		t.Fatal(err)
	}
	if err := (events.Consumer{Redis: controlRedis, Consumer: "control-trim", Handler: &acceptanceHandler{}, Now: func() time.Time { return now }}).Trim(ctx); err != nil {
		t.Fatal(err)
	}
	entries, err := admin.XRange(ctx, events.StreamKey, "-", "+").Result()
	if err != nil || len(entries) != 1 || entries[0].ID != freshID {
		t.Fatalf("retention trim entries=%v err=%v", entries, err)
	}
}

func redisACLDenied(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToUpper(err.Error())
	return strings.Contains(message, "NOPERM") || strings.Contains(message, "USER EXECUTING THE SCRIPT CAN'T RUN THIS COMMAND")
}

func acceptanceDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL is not set")
	}
	db, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := migrations.Apply(ctx, db); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `TRUNCATE migration_records, migration_runs, usage_ledger, request_attempts, credentials, accounts RESTART IDENTITY CASCADE`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO accounts (id,provider,platform,source_system,source_id) VALUES ('account-acceptance','apikey','apikey','acceptance','account-acceptance')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	return db
}

func startCrashProcess(t *testing.T, mode, marker string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAcceptanceCrashHelper$")
	cmd.Env = append(os.Environ(), crashModeEnv+"="+mode)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line := make(chan string, 1)
	go func() {
		value, _ := bufio.NewReader(stdout).ReadString('\n')
		line <- strings.TrimSpace(value)
	}()
	select {
	case got := <-line:
		if got != marker {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("crash helper marker=%q want=%q stderr=%s", got, marker, stderr.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("crash helper timed out: %s", stderr.String())
	}
	return cmd
}

func killCrashProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("crash helper exited normally after SIGKILL")
	}
}

func TestAcceptanceControlCrashAfterCommitBeforeAck(t *testing.T) {
	cfg := acceptanceRedisConfig(t)
	db := acceptanceDatabase(t)
	defer db.Close()
	ctx := context.Background()
	admin := redisClient(cfg.addr, cfg.adminUser, cfg.adminPassword)
	gatewayRedis := redisClient(cfg.addr, cfg.gatewayUser, cfg.gatewayPassword)
	controlRedis := redisClient(cfg.addr, cfg.controlUser, cfg.controlPassword)
	defer admin.Close()
	defer gatewayRedis.Close()
	defer controlRedis.Close()
	if err := admin.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	attempt := acceptanceAttempt(time.Now().UTC(), "control-crash")
	messageID, err := (events.Producer{Redis: gatewayRedis, ProducerID: attempt.ProducerID}).Add(ctx, attemptEvent(t, attempt))
	if err != nil {
		t.Fatal(err)
	}
	helper := startCrashProcess(t, controlCrashMode, "committed")
	killCrashProcess(t, helper)
	var attempts int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM request_attempts WHERE attempt_id=$1`, attempt.AttemptID).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("committed attempt count=%d err=%v", attempts, err)
	}
	if pending := controlRedis.XPending(ctx, events.StreamKey, events.GroupName).Val().Count; pending != 1 {
		t.Fatalf("pending after control crash=%d", pending)
	}
	forcePendingIdle(t, ctx, controlRedis, messageID)
	ledger := control.Ledger{DB: db}
	if err := (events.Consumer{Redis: controlRedis, Consumer: "control-reclaimer", Handler: ledger, Now: time.Now}).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if pending := controlRedis.XPending(ctx, events.StreamKey, events.GroupName).Val().Count; pending != 0 {
		t.Fatalf("pending after reclaim=%d", pending)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM request_attempts WHERE attempt_id=$1`, attempt.AttemptID).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("attempt idempotency count=%d err=%v", attempts, err)
	}
}

func TestAcceptanceGatewayCrashAfterAttemptStartedRecovery(t *testing.T) {
	cfg := acceptanceRedisConfig(t)
	db := acceptanceDatabase(t)
	defer db.Close()
	ctx := context.Background()
	admin := redisClient(cfg.addr, cfg.adminUser, cfg.adminPassword)
	controlRedis := redisClient(cfg.addr, cfg.controlUser, cfg.controlPassword)
	defer admin.Close()
	defer controlRedis.Close()
	if err := admin.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	helper := startCrashProcess(t, gatewayCrashMode, "attempt_started")
	killCrashProcess(t, helper)
	ledger := control.Ledger{DB: db}
	if err := (events.Consumer{Redis: controlRedis, Consumer: "control-recovery", Handler: ledger, Now: time.Now}).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var state, source string
	var partial bool
	if err := db.QueryRow(ctx, `SELECT a.state,l.usage_source,l.partial FROM request_attempts a JOIN usage_ledger l USING (attempt_id) WHERE a.attempt_id='attempt-gateway-crash'`).Scan(&state, &source, &partial); err != nil {
		t.Fatal(err)
	}
	if state != "reconciled" || source != contracts.UsageSourceMissing || !partial {
		t.Fatalf("recovery terminal: state=%s source=%s partial=%v", state, source, partial)
	}
	if recovered, err := ledger.RecoverAttempts(ctx, time.Now()); err != nil || recovered != 0 {
		t.Fatalf("duplicate recovery: recovered=%d err=%v", recovered, err)
	}
	var ledgerRows int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM usage_ledger WHERE attempt_id='attempt-gateway-crash'`).Scan(&ledgerRows); err != nil || ledgerRows != 1 {
		t.Fatalf("recovery ledger rows=%d err=%v", ledgerRows, err)
	}
}

type crashAfterCommitHandler struct{ ledger control.Ledger }

func (h crashAfterCommitHandler) HandleAttemptStarted(ctx context.Context, attempt contracts.AttemptStarted) error {
	if err := h.ledger.HandleAttemptStarted(ctx, attempt); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "committed")
	select {}
}

func (h crashAfterCommitHandler) HandleRelease(context.Context, contracts.Release) error { return nil }
func (h crashAfterCommitHandler) RecoverAttempts(context.Context, time.Time) (int, error) {
	return 0, nil
}

func TestAcceptanceCrashHelper(t *testing.T) {
	mode := os.Getenv(crashModeEnv)
	if mode == "" {
		return
	}
	cfg := acceptanceRedis{
		addr:            os.Getenv("GATEWAY_TEST_REDIS_ADDR"),
		gatewayUser:     os.Getenv("GATEWAY_TEST_REDIS_GATEWAY_USERNAME"),
		gatewayPassword: os.Getenv("GATEWAY_TEST_REDIS_GATEWAY_PASSWORD"),
		controlUser:     os.Getenv("GATEWAY_TEST_REDIS_CONTROL_USERNAME"),
		controlPassword: os.Getenv("GATEWAY_TEST_REDIS_CONTROL_PASSWORD"),
	}
	ctx := context.Background()
	switch mode {
	case controlCrashMode:
		db, err := pgxpool.New(ctx, os.Getenv("GATEWAY_TEST_DATABASE_URL"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		rdb := redisClient(cfg.addr, cfg.controlUser, cfg.controlPassword)
		defer rdb.Close()
		handler := crashAfterCommitHandler{ledger: control.Ledger{DB: db}}
		if err := (events.Consumer{Redis: rdb, Consumer: "control-crash", Handler: handler, Now: time.Now}).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	case gatewayCrashMode:
		rdb := redisClient(cfg.addr, cfg.gatewayUser, cfg.gatewayPassword)
		defer rdb.Close()
		occurredAt := time.Now().UTC().Add(-10 * time.Minute)
		attempt := acceptanceAttempt(occurredAt, "gateway-crash")
		attempt.DeadlineAt = occurredAt.Add(time.Minute)
		if _, err := (events.Producer{Redis: rdb, ProducerID: attempt.ProducerID}).Add(ctx, attemptEvent(t, attempt)); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(os.Stdout, "attempt_started")
		select {}
	default:
		t.Fatalf("unknown crash helper mode %q", mode)
	}
}
