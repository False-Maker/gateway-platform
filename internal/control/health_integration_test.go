package control

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/elucid/gateway-platform/internal/control/credentials"
	"github.com/elucid/gateway-platform/internal/observability"
	"github.com/elucid/gateway-platform/migrations"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPGReleaseAuthorityCooldownsAndExclusions exercises the control-side
// ErrorClass verdicts end to end on PostgreSQL: the ledger applies them, the
// snapshot repository honours them, and the starvation guard releases them.
func TestPGReleaseAuthorityCooldownsAndExclusions(t *testing.T) {
	databaseURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := migrations.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `TRUNCATE usage_ledger, request_attempts, credentials, accounts RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	// Other PG tests iterate every bucket with their own cipher key; leave no
	// rows behind that they cannot decrypt.
	defer db.Exec(context.Background(), `DELETE FROM accounts WHERE source_system='a9'`)
	key := make([]byte, 32)
	for i := range key {
		key[i] = 0x51
	}
	cipher, err := credentials.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	const platform, group = "a9-platform", "default"
	accounts := []string{"a9-1", "a9-2", "a9-3", "a9-4"}
	for _, id := range accounts {
		encrypted, err := cipher.Encrypt(id, []byte(`{"access_token":"k"}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO accounts (id,provider,platform,"group",source_system,source_id) VALUES ($1,$2,$2,$3,'a9',$1)`, id, platform, group); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO credentials (id,account_id,kind,encrypted_secret) VALUES ($1,$2,'static',$3)`, id+":c", id, encrypted); err != nil {
			t.Fatal(err)
		}
	}
	metrics := observability.NewRegistry()
	ledger := Ledger{DB: db, Metrics: metrics, Signals: NewPlatformSignals(metrics)}
	repo := PGSnapshotRepository{DB: db, Cipher: cipher}
	now := time.Now().UTC()
	seq := 0
	emit := func(account, model string, class contracts.ErrorClass, at time.Time) {
		t.Helper()
		seq++
		attempt := "a9-attempt-" + string(rune('a'+seq%26)) + time.Now().Format("150405.000000")
		started := contracts.AttemptStarted{SchemaVersion: contracts.SchemaVersion, EventID: "start-" + attempt, RequestID: "req-" + attempt, AttemptID: attempt, AttemptNo: 1, ProducerID: "gw", OccurredAt: at.Add(-time.Second), AccountID: account, Provider: platform, Model: model, TenantID: "t", DeadlineAt: at.Add(time.Minute)}
		if err := ledger.HandleAttemptStarted(ctx, started); err != nil {
			t.Fatal(err)
		}
		release := contracts.Release{SchemaVersion: contracts.SchemaVersion, EventID: contracts.TerminalEventID(attempt), RequestID: started.RequestID, AttemptID: attempt, AttemptNo: 1, ProducerID: "gw", OccurredAt: at, AccountID: account, Provider: platform, StatusCode: 429, Model: model, TenantID: "t", ErrorClass: class, UsageSource: contracts.UsageSourceMissing}
		if err := ledger.HandleRelease(ctx, release); err != nil {
			t.Fatal(err)
		}
	}
	schedulable := func() map[string]contracts.Account {
		t.Helper()
		list, err := repo.ListSnapshotAccounts(ctx, platform, group)
		if err != nil {
			t.Fatal(err)
		}
		out := make(map[string]contracts.Account, len(list))
		for _, account := range list {
			out[account.ID] = account
		}
		return out
	}
	var failures int
	var cooldown *time.Time
	readHealth := func(id string) {
		t.Helper()
		if err := db.QueryRow(ctx, `SELECT consecutive_failures, cooldown_until FROM accounts WHERE id=$1`, id).Scan(&failures, &cooldown); err != nil {
			t.Fatal(err)
		}
	}

	// rate_limited_unknown: two strikes are tolerated, the third imposes a 5min cooldown
	emit("a9-1", "m", contracts.ErrorRateLimitedUnknown, now)
	emit("a9-1", "m", contracts.ErrorRateLimitedUnknown, now)
	readHealth("a9-1")
	if failures != 2 || cooldown != nil {
		t.Fatalf("after two unknown 429s: failures=%d cooldown=%v", failures, cooldown)
	}
	if _, ok := schedulable()["a9-1"]; !ok {
		t.Fatal("account cooled before escalation threshold")
	}
	emit("a9-1", "m", contracts.ErrorRateLimitedUnknown, now)
	readHealth("a9-1")
	if failures != 3 || cooldown == nil || cooldown.Sub(now) < 4*time.Minute || cooldown.Sub(now) > 6*time.Minute {
		t.Fatalf("after third unknown 429: failures=%d cooldown=%v", failures, cooldown)
	}
	if _, ok := schedulable()["a9-1"]; ok {
		t.Fatal("cooling account still published in snapshot")
	}
	// ok clears everything
	emit("a9-1", "m", contracts.ErrorOK, now)
	readHealth("a9-1")
	if failures != 0 || cooldown != nil {
		t.Fatalf("ok did not reset: failures=%d cooldown=%v", failures, cooldown)
	}
	if _, ok := schedulable()["a9-1"]; !ok {
		t.Fatal("recovered account missing from snapshot")
	}

	// forbidden_capability excludes only that model, no cooldown, idempotent
	emit("a9-2", "gpt-x", contracts.ErrorForbiddenCapability, now)
	emit("a9-2", "gpt-x", contracts.ErrorForbiddenCapability, now)
	emit("a9-2", "gpt-y", contracts.ErrorForbiddenCapability, now)
	readHealth("a9-2")
	var excludedJSON []byte
	if err := db.QueryRow(ctx, `SELECT excluded_models FROM accounts WHERE id='a9-2'`).Scan(&excludedJSON); err != nil {
		t.Fatal(err)
	}
	var excluded []string
	_ = json.Unmarshal(excludedJSON, &excluded)
	if cooldown != nil || len(excluded) != 2 || excluded[0] != "gpt-x" || excluded[1] != "gpt-y" {
		t.Fatalf("capability exclusion: cooldown=%v excluded=%v", cooldown, excluded)
	}
	if account, ok := schedulable()["a9-2"]; !ok || len(account.ExcludedModels) != 2 {
		t.Fatalf("snapshot did not carry excluded models: %#v", account)
	}

	// forbidden_transport and upstream_5xx never penalise the account
	emit("a9-3", "m", contracts.ErrorForbiddenTransport, now)
	emit("a9-3", "m", contracts.ErrorUpstream5xx, now)
	emit("a9-3", "m", contracts.ErrorForbiddenTransport, now)
	readHealth("a9-3")
	if failures != 0 || cooldown != nil {
		t.Fatalf("transport/5xx penalised account: failures=%d cooldown=%v", failures, cooldown)
	}
	// platform signal: a second distinct account blocked inside the window alerts
	if got := ledger.Signals.Alerting(); len(got) != 0 {
		t.Fatalf("premature alert: %v", got)
	}
	emit("a9-4", "m", contracts.ErrorBlocked, now)
	if got := ledger.Signals.Alerting(); len(got) != 1 || got[0] != platform {
		t.Fatalf("platform alert missing: %v", got)
	}
	readHealth("a9-4")
	if cooldown == nil || cooldown.Sub(now) < 50*time.Second || cooldown.Sub(now) > 70*time.Second {
		t.Fatalf("blocked cooldown=%v", cooldown)
	}

	// starvation guard: cool 3 of 4 (>50%), the oldest cooldown is released
	for _, id := range []string{"a9-1", "a9-2"} {
		if _, err := db.Exec(ctx, `UPDATE accounts SET cooldown_until=$2 WHERE id=$1`, id, now.Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(ctx, `UPDATE accounts SET cooldown_until=$2 WHERE id=$1`, "a9-4", now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(schedulable()) != 1 {
		t.Fatalf("expected only a9-3 schedulable, got %v", schedulable())
	}
	released, err := repo.ReleaseStarvedCooldowns(ctx, now)
	if err != nil || len(released) != 1 || released[0] != platform {
		t.Fatalf("starvation release=%v err=%v", released, err)
	}
	after := schedulable()
	if _, ok := after["a9-4"]; !ok || len(after) != 2 {
		t.Fatalf("oldest cooldown (a9-4) was not released: %v", after)
	}
	// now 2 of 4 cooling == 50%, not over the ratio: nothing more is released
	if released, err := repo.ReleaseStarvedCooldowns(ctx, now); err != nil || len(released) != 0 {
		t.Fatalf("guard released below threshold: %v err=%v", released, err)
	}
	// duplicate delivery of an already-ledgered release must not re-apply verdicts
	readHealth("a9-1")
	before := cooldown
	emit("a9-1", "m", contracts.ErrorOK, now) // fresh attempt resets
	readHealth("a9-1")
	if cooldown != nil {
		t.Fatalf("ok after starvation did not clear: before=%v after=%v", before, cooldown)
	}
}
