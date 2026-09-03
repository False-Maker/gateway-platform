package control

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresLedgerIdempotencyAndRecovery(t *testing.T) {
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
	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "001_initial.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `TRUNCATE migration_records, migration_runs, usage_ledger, request_attempts, credentials, accounts RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO accounts (id,provider,platform,source_system,source_id) VALUES ('account-1','apikey','apikey','test','account-1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO credentials (id,account_id,kind,encrypted_secret) VALUES ('credential-1','account-1','static',$1)`, []byte("old-secret")); err != nil {
		t.Fatal(err)
	}
	fencer := Fencer{DB: db}
	staleEpoch, err := fencer.Acquire(ctx, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	currentEpoch, err := fencer.Acquire(ctx, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := fencer.UpdateCredential(ctx, "account-1", staleEpoch, []byte("stale-secret"), nil); !errors.Is(err, ErrFenceLost) {
		t.Fatalf("stale fencing writer was accepted: %v", err)
	}
	if err := fencer.UpdateCredential(ctx, "account-1", currentEpoch, []byte("new-secret"), nil); err != nil {
		t.Fatal(err)
	}
	var encryptedSecret []byte
	var credentialVersion int64
	if err := db.QueryRow(ctx, `SELECT encrypted_secret,version FROM credentials WHERE account_id='account-1'`).Scan(&encryptedSecret, &credentialVersion); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encryptedSecret, []byte("new-secret")) || credentialVersion != 1 {
		t.Fatalf("credential fencing result: secret=%q version=%d", encryptedSecret, credentialVersion)
	}
	unlock, acquired, err := fencer.TryAcquireSingleton(ctx, 991337)
	if err != nil || !acquired || unlock == nil {
		t.Fatalf("singleton acquire: acquired=%v err=%v", acquired, err)
	}
	if _, acquired, err := fencer.TryAcquireSingleton(ctx, 991337); err != nil || acquired {
		t.Fatalf("contended singleton acquire: acquired=%v err=%v", acquired, err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	ledger := Ledger{DB: db}
	now := time.Now().UTC()
	started := contracts.AttemptStarted{SchemaVersion: contracts.SchemaVersion, EventID: "start-1", RequestID: "request-1", AttemptID: "attempt-1", AttemptNo: 1, ProducerID: "gateway-1", OccurredAt: now.Add(-time.Minute), AccountID: "account-1", Provider: "apikey", Model: "m", TenantID: "tenant", DeadlineAt: now.Add(time.Minute)}
	if err := ledger.HandleAttemptStarted(ctx, started); err != nil {
		t.Fatal(err)
	}
	release := contracts.Release{SchemaVersion: contracts.SchemaVersion, EventID: contracts.TerminalEventID(started.AttemptID), RequestID: started.RequestID, AttemptID: started.AttemptID, AttemptNo: 1, ProducerID: "gateway-1", OccurredAt: now, AccountID: started.AccountID, Provider: started.Provider, StatusCode: 401, Model: started.Model, TenantID: started.TenantID, ErrorClass: contracts.ErrorAuthInvalid, UsageSource: contracts.UsageSourceMissing}
	if err := ledger.HandleRelease(ctx, release); err != nil {
		t.Fatal(err)
	}
	if err := ledger.HandleRelease(ctx, release); err != nil {
		t.Fatal(err)
	}
	var ledgerCount int
	var state, accountStatus string
	if err := db.QueryRow(ctx, `SELECT count(*) FROM usage_ledger WHERE attempt_id=$1`, started.AttemptID).Scan(&ledgerCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT state FROM request_attempts WHERE attempt_id=$1`, started.AttemptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT status FROM accounts WHERE id=$1`, started.AccountID).Scan(&accountStatus); err != nil {
		t.Fatal(err)
	}
	if ledgerCount != 1 || state != "terminal" || accountStatus != "disabled" {
		t.Fatalf("idempotent release state: count=%d attempt=%s account=%s", ledgerCount, state, accountStatus)
	}
	expired := started
	expired.EventID, expired.RequestID, expired.AttemptID = "start-2", "request-2", "attempt-2"
	expired.DeadlineAt = now.Add(-time.Second)
	if err := ledger.HandleAttemptStarted(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if recovered, err := ledger.RecoverAttempts(ctx, now); err != nil || recovered != 1 {
		t.Fatalf("recover attempts: recovered=%d err=%v", recovered, err)
	}
	if recovered, err := ledger.RecoverAttempts(ctx, now.Add(time.Minute)); err != nil || recovered != 0 {
		t.Fatalf("duplicate recovery: recovered=%d err=%v", recovered, err)
	}
	var source string
	var partial bool
	if err := db.QueryRow(ctx, `SELECT usage_source,partial FROM usage_ledger WHERE attempt_id=$1`, expired.AttemptID).Scan(&source, &partial); err != nil {
		t.Fatal(err)
	}
	if source != contracts.UsageSourceMissing || !partial {
		t.Fatalf("synthetic terminal: source=%s partial=%v", source, partial)
	}
}
