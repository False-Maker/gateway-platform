package newapi

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/elucid/gateway-platform/internal/control/credentials"
	"github.com/elucid/gateway-platform/migrations"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Apply is the migration's only write path into the target database. The
// invariants under test: one run is atomic, re-running is idempotent while
// still bumping the fence, and no plaintext credential reaches a column.

const (
	testClaudeKey   = "sk-claude-plaintext-do-not-store"
	testCodexAccess = "codex-access-plaintext"
	testCodexRefres = "codex-refresh-plaintext"
)

func openTargetTestPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := migrations.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `TRUNCATE migration_records, migration_runs, usage_ledger, request_attempts, credentials, accounts RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	// Clean up on the way out as well. The test database is shared across
	// packages under `go test -p 1`, and an account left behind here carries a
	// credential encrypted with this package's test key -- any other package
	// that builds a snapshot over all accounts would fail to decrypt it.
	t.Cleanup(func() {
		background := context.Background()
		for _, statement := range []string{
			`DELETE FROM accounts WHERE source_system=$1`,
			`DELETE FROM migration_runs WHERE source_system=$1`,
		} {
			if _, err := db.Exec(background, statement, SourceSystem); err != nil {
				t.Errorf("clean up %q: %v", statement, err)
			}
		}
	})
	return ctx, db
}

func testCipher(t *testing.T) *credentials.Cipher {
	t.Helper()
	cipher, err := credentials.NewCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}

// applyTestPlan covers both credential shapes the migration supports: a static
// API key channel with a proxy and two groups, and a codex OAuth channel.
func applyTestPlan(t *testing.T) Plan {
	t.Helper()
	plan, err := BuildPlan(SourceSnapshot{
		Channels: []SourceChannel{
			{
				ID: 7, Type: 14, Key: testClaudeKey, Status: 1, Name: "claude-a",
				Models: `["m1","m2"]`, Group: "default,vip",
				Setting: `{"proxy":"http://p.example.test:8080"}`, ModelMapping: `{"m1":"m2"}`,
			},
			{
				ID: 9, Type: 57, Status: 1, Name: "codex-a", Group: "default",
				Key: `{"access_token":"` + testCodexAccess + `","refresh_token":"` + testCodexRefres + `","account_id":"acct-1","email":"a@example.test"}`,
			},
		},
		Users:  []SourceUser{{ID: 3, Username: "alice", Status: 1, Quota: "500000"}},
		Tokens: []SourceToken{{ID: 11, UserID: 3, Status: 1, Key: "plaintext-token-key"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// default+vip on channel 7, plus channel 9 => three accounts.
	if len(plan.Accounts) != 3 {
		t.Fatalf("fixture drifted: got %d planned accounts, want 3", len(plan.Accounts))
	}
	return plan
}

func TestApplyWritesAccountsCredentialsAndRecordsInOneRun(t *testing.T) {
	ctx, db := openTargetTestPool(t)
	cipher := testCipher(t)
	plan := applyTestPlan(t)

	if err := Apply(ctx, db, cipher, plan, "run-1"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var status string
	var completed *string
	if err := db.QueryRow(ctx, `SELECT status, completed_at::text FROM migration_runs WHERE id='run-1'`).Scan(&status, &completed); err != nil {
		t.Fatal(err)
	}
	if status != "completed" {
		t.Errorf("migration run status = %q, want completed", status)
	}
	if completed == nil {
		t.Error("completed_at was not stamped")
	}

	var accountCount int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE source_system=$1`, SourceSystem).Scan(&accountCount); err != nil {
		t.Fatal(err)
	}
	if accountCount != len(plan.Accounts) {
		t.Errorf("got %d accounts, want %d", accountCount, len(plan.Accounts))
	}

	// Every planned account must be reachable by its deterministic id, carry
	// its group, and be linked to a credential row.
	for _, planned := range plan.Accounts {
		var group, credentialID, proxy string
		var epoch int64
		var modelMapping []byte
		err := db.QueryRow(ctx, `SELECT "group", COALESCE(credential_id,''), COALESCE(proxy,''), fence_epoch, model_mapping FROM accounts WHERE id=$1 AND source_id=$2`,
			planned.Account.ID, planned.SourceID).Scan(&group, &credentialID, &proxy, &epoch, &modelMapping)
		if err != nil {
			t.Fatalf("account for source %s: %v", planned.SourceID, err)
		}
		if group != planned.Account.Group {
			t.Errorf("account %s group = %q, want %q", planned.SourceID, group, planned.Account.Group)
		}
		if credentialID != planned.Account.ID+":credential" {
			t.Errorf("account %s credential_id = %q", planned.SourceID, credentialID)
		}
		if epoch != 1 {
			t.Errorf("first apply left account %s at fence_epoch %d, want 1", planned.SourceID, epoch)
		}
		if proxy != planned.Proxy {
			t.Errorf("account %s proxy = %q, want %q", planned.SourceID, proxy, planned.Proxy)
		}
		if !json.Valid(modelMapping) {
			t.Errorf("account %s model_mapping is not valid JSON: %s", planned.SourceID, modelMapping)
		}

		// The stored credential must decrypt back to the exact bundle, and it
		// must be bound to this account id (AES-GCM additional data).
		var encrypted []byte
		var kind string
		if err := db.QueryRow(ctx, `SELECT encrypted_secret, kind FROM credentials WHERE account_id=$1`, planned.Account.ID).Scan(&encrypted, &kind); err != nil {
			t.Fatalf("credential for %s: %v", planned.SourceID, err)
		}
		if kind != planned.Account.Credential.Kind {
			t.Errorf("credential kind = %q, want %q", kind, planned.Account.Credential.Kind)
		}
		decrypted, err := cipher.Decrypt(planned.Account.ID, encrypted)
		if err != nil {
			t.Fatalf("decrypt credential for %s: %v", planned.SourceID, err)
		}
		var bundle contracts.TokenBundle
		if err := json.Unmarshal(decrypted, &bundle); err != nil {
			t.Fatal(err)
		}
		if bundle.AccessToken != planned.Bundle.AccessToken || bundle.RefreshToken != planned.Bundle.RefreshToken {
			t.Errorf("credential for %s did not round-trip", planned.SourceID)
		}
		// A credential must not decrypt under a different account id.
		if _, err := cipher.Decrypt("account-someone-else", encrypted); err == nil {
			t.Errorf("credential for %s decrypted under a foreign account id", planned.SourceID)
		}
	}

	// The staging records A5 produces must all land, tagged with this run.
	var recordCount int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM migration_records WHERE run_id='run-1'`).Scan(&recordCount); err != nil {
		t.Fatal(err)
	}
	if recordCount != len(plan.Records) {
		t.Errorf("got %d migration records, want %d", recordCount, len(plan.Records))
	}
	var tenantTarget, tokenTarget string
	if err := db.QueryRow(ctx, `SELECT COALESCE(target_id,'') FROM migration_records WHERE record_kind='tenant' AND source_id='user:3'`).Scan(&tenantTarget); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT COALESCE(target_id,'') FROM migration_records WHERE record_kind='token' AND source_id='token:11'`).Scan(&tokenTarget); err != nil {
		t.Fatal(err)
	}
	if tenantTarget == "" || tokenTarget == "" {
		t.Errorf("user/token staging records lost their target ids: %q %q", tenantTarget, tokenTarget)
	}
}

func TestApplyNeverWritesPlaintextSecretsToAnyColumn(t *testing.T) {
	ctx, db := openTargetTestPool(t)
	if err := Apply(ctx, db, testCipher(t), applyTestPlan(t), "run-1"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Dump every row of the three tables Apply writes as text and search for
	// the fixture secrets. This is deliberately blunt: it catches a plaintext
	// leak into profile, raw_summary or conversion_result, not just into
	// encrypted_secret.
	secrets := []string{testClaudeKey, testCodexAccess, testCodexRefres, "plaintext-token-key"}
	for _, query := range []string{
		`SELECT accounts::text FROM accounts`,
		`SELECT credentials::text FROM credentials`,
		`SELECT migration_records::text FROM migration_records`,
		`SELECT migration_runs::text FROM migration_runs`,
	} {
		rows, err := db.Query(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var dumped string
			if err := rows.Scan(&dumped); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			for _, secret := range secrets {
				if strings.Contains(dumped, secret) {
					rows.Close()
					t.Fatalf("plaintext secret %q found in a row from %q", secret, query)
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}

	// Sanity check the search would actually have found something: the digest
	// of the claude key is expected to be present in the staging summary.
	var digestHits int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM migration_records WHERE raw_summary::text LIKE '%'||$1||'%'`, digestString(testClaudeKey)).Scan(&digestHits); err != nil {
		t.Fatal(err)
	}
	if digestHits == 0 {
		t.Error("no record carries the key digest, so the plaintext scan above proves little")
	}
}

func TestApplyIsIdempotentAndBumpsFenceAndCredentialVersion(t *testing.T) {
	ctx, db := openTargetTestPool(t)
	cipher := testCipher(t)
	plan := applyTestPlan(t)

	if err := Apply(ctx, db, cipher, plan, "run-1"); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	// A re-run uses a new run id; the source ids are unchanged, so rows must be
	// updated in place rather than duplicated.
	if err := Apply(ctx, db, cipher, plan, "run-2"); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	var accountCount, recordCount, credentialCount int
	if err := db.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM accounts WHERE source_system=$1),
			(SELECT count(*) FROM migration_records WHERE source_system=$1),
			(SELECT count(*) FROM credentials)`, SourceSystem).Scan(&accountCount, &recordCount, &credentialCount); err != nil {
		t.Fatal(err)
	}
	if accountCount != len(plan.Accounts) {
		t.Errorf("re-apply produced %d accounts, want %d", accountCount, len(plan.Accounts))
	}
	if credentialCount != len(plan.Accounts) {
		t.Errorf("re-apply produced %d credentials, want %d", credentialCount, len(plan.Accounts))
	}
	if recordCount != len(plan.Records) {
		t.Errorf("re-apply produced %d records, want %d", recordCount, len(plan.Records))
	}

	// Fence epoch must advance on every apply: a re-import invalidates any
	// in-flight lease that still holds the old credential.
	for _, planned := range plan.Accounts {
		var epoch, version int64
		if err := db.QueryRow(ctx, `SELECT a.fence_epoch, c.version FROM accounts a JOIN credentials c ON c.account_id=a.id WHERE a.id=$1`, planned.Account.ID).Scan(&epoch, &version); err != nil {
			t.Fatal(err)
		}
		if epoch != 2 {
			t.Errorf("account %s fence_epoch = %d after two applies, want 2", planned.SourceID, epoch)
		}
		if version != 1 {
			t.Errorf("account %s credential version = %d after two applies, want 1", planned.SourceID, version)
		}
	}

	// Records must be re-tagged to the latest run, not left pointing at run-1.
	var staleRunRecords int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM migration_records WHERE run_id<>'run-2'`).Scan(&staleRunRecords); err != nil {
		t.Fatal(err)
	}
	if staleRunRecords != 0 {
		t.Errorf("%d records still point at the previous run", staleRunRecords)
	}
}

func TestApplyRollsBackTheWholeRunOnFailure(t *testing.T) {
	ctx, db := openTargetTestPool(t)
	cipher := testCipher(t)
	plan := applyTestPlan(t)

	if err := Apply(ctx, db, cipher, plan, "run-1"); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	var epochBefore int64
	if err := db.QueryRow(ctx, `SELECT fence_epoch FROM accounts WHERE id=$1`, plan.Accounts[0].Account.ID).Scan(&epochBefore); err != nil {
		t.Fatal(err)
	}

	// Reusing a run id violates the migration_runs primary key. The failure
	// happens on the first statement of the transaction, and everything after
	// it must be discarded -- a half-applied migration would leave accounts
	// bumped past the fence their published credential was encrypted for.
	err := Apply(ctx, db, cipher, plan, "run-1")
	if err == nil {
		t.Fatal("Apply succeeded with a duplicate run id")
	}
	if !strings.Contains(err.Error(), "insert migration run") {
		t.Errorf("unexpected failure point: %v", err)
	}

	var runCount int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM migration_runs`).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if runCount != 1 {
		t.Errorf("got %d migration runs, want 1", runCount)
	}
	var epochAfter int64
	if err := db.QueryRow(ctx, `SELECT fence_epoch FROM accounts WHERE id=$1`, plan.Accounts[0].Account.ID).Scan(&epochAfter); err != nil {
		t.Fatal(err)
	}
	if epochAfter != epochBefore {
		t.Errorf("a failed run still moved fence_epoch from %d to %d", epochBefore, epochAfter)
	}
}

func TestApplyGeneratesARunIDWhenNoneIsSupplied(t *testing.T) {
	ctx, db := openTargetTestPool(t)
	if err := Apply(ctx, db, testCipher(t), applyTestPlan(t), ""); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var runID string
	if err := db.QueryRow(ctx, `SELECT id FROM migration_runs`).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(runID, "new-api-") {
		t.Errorf("generated run id = %q, want a new-api- prefix", runID)
	}
	var recordsWithRun int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM migration_records WHERE run_id=$1`, runID).Scan(&recordsWithRun); err != nil {
		t.Fatal(err)
	}
	if recordsWithRun == 0 {
		t.Error("records were not tagged with the generated run id")
	}
}
