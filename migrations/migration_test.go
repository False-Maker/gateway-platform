package migrations

import (
	"os"
	"strings"
	"testing"
)

func TestInitialSchemaContainsP0TablesAndConstraints(t *testing.T) {
	data, err := os.ReadFile("001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	schema := string(data)
	for _, table := range []string{"accounts", "credentials", "request_attempts", "usage_ledger", "migration_runs", "migration_records", "quota_snapshot_outbox"} {
		if !strings.Contains(schema, "CREATE TABLE IF NOT EXISTS "+table) {
			t.Errorf("missing table %s", table)
		}
	}
	for _, constraint := range []string{"event_id TEXT NOT NULL UNIQUE", "attempt_id TEXT NOT NULL UNIQUE", "terminal_event_id TEXT UNIQUE", "fence_epoch BIGINT NOT NULL"} {
		if !strings.Contains(schema, constraint) {
			t.Errorf("missing schema invariant %q", constraint)
		}
	}
	if strings.Contains(schema, "access_token TEXT") {
		t.Fatal("credentials must not persist plaintext access tokens")
	}
}

func TestBillingSchemaKeepsMoneyExactAndPricesImmutable(t *testing.T) {
	data, err := os.ReadFile("005_billing.sql")
	if err != nil {
		t.Fatal(err)
	}
	schema := string(data)
	for _, table := range []string{"model_prices", "tenant_wallets", "wallet_transactions"} {
		if !strings.Contains(schema, "CREATE TABLE IF NOT EXISTS "+table) {
			t.Errorf("missing table %s", table)
		}
	}
	// Every money column is NUMERIC. A DOUBLE PRECISION / REAL column here
	// would make B4.5's "explain the difference down to an event_id"
	// impossible, so it is a schema invariant, not a style preference.
	for _, column := range []string{
		"price_input NUMERIC(38,12) NOT NULL",
		"price_output NUMERIC(38,12) NOT NULL",
		"price_cache_read NUMERIC(38,12) NOT NULL",
		"price_cache_write NUMERIC(38,12) NOT NULL",
		"balance NUMERIC(38,12) NOT NULL DEFAULT 0",
		"amount NUMERIC(38,12) NOT NULL",
		"balance_after NUMERIC(38,12) NOT NULL",
	} {
		if !strings.Contains(schema, column) {
			t.Errorf("missing exact-decimal column %q", column)
		}
	}
	var statements []string
	for _, line := range strings.Split(strings.ToUpper(schema), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			statements = append(statements, line)
		}
	}
	declarations := strings.Join(statements, "\n")
	for _, forbidden := range []string{"DOUBLE PRECISION", " REAL", " FLOAT"} {
		if strings.Contains(declarations, forbidden) {
			t.Errorf("money column uses %s instead of NUMERIC", strings.TrimSpace(forbidden))
		}
	}
	// D3: a price row may never be rewritten, in the application or by hand.
	if !strings.Contains(schema, "BEFORE UPDATE OR DELETE ON model_prices") {
		t.Error("model_prices is missing its immutability trigger")
	}
	if !strings.Contains(schema, "UNIQUE (provider, model, effective_from)") {
		t.Error("model_prices must be unique per provider/model/effective_from")
	}
	if !strings.Contains(schema, "kind IN ('topup', 'debit', 'adjustment')") {
		t.Error("wallet_transactions must constrain its kinds")
	}
}

func TestUsageBillingSchemaIsAdditiveAndIdempotent(t *testing.T) {
	data, err := os.ReadFile("006_usage_billing.sql")
	if err != nil {
		t.Fatal(err)
	}
	schema := string(data)
	// usage_ledger already exists and is written by the hot path's event
	// consumer, so 006 may only add columns, never redefine the table.
	if strings.Contains(schema, "CREATE TABLE IF NOT EXISTS usage_ledger") || strings.Contains(schema, "DROP TABLE") {
		t.Error("006 must extend usage_ledger, not recreate it")
	}
	for _, column := range []string{"billing_state TEXT NOT NULL DEFAULT 'pending'", "billed_amount NUMERIC(38,12)", "billing_run_id TEXT"} {
		if !strings.Contains(schema, "ADD COLUMN IF NOT EXISTS "+column) {
			t.Errorf("missing idempotent column %q", column)
		}
	}
	// ALTER TABLE ... ADD CONSTRAINT has no IF NOT EXISTS, so a re-apply has
	// to be guarded explicitly or the second boot fails.
	if !strings.Contains(schema, "FROM pg_constraint WHERE conname = 'usage_ledger_billing_state_check'") {
		t.Error("the billing_state constraint is not guarded against a re-apply")
	}
	if !strings.Contains(schema, "billing_state IN ('pending', 'billed', 'unpriced', 'held')") {
		t.Error("billing_state must be constrained to the documented values")
	}
	if !strings.Contains(schema, "CREATE TABLE IF NOT EXISTS billing_runs") {
		t.Error("missing billing_runs table")
	}
	if !strings.Contains(schema, "total_debited NUMERIC(38,12)") {
		t.Error("billing_runs.total_debited must be exact decimal")
	}
}
