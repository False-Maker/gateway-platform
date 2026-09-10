package control

import (
	"context"
	"testing"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

// purgeAllTestPrices empties the price table. The immutability trigger has to
// be disabled to do it, which is the point of the trigger; the test database
// is disposable and the gated suite runs with -p 1, so no other test is
// looking at these rows at the same time.
func purgeAllTestPrices(ctx context.Context, db *pgxpool.Pool) error {
	if _, err := db.Exec(ctx, `ALTER TABLE model_prices DISABLE TRIGGER model_prices_immutable_trigger`); err != nil {
		return err
	}
	_, deleteErr := db.Exec(ctx, `DELETE FROM model_prices`)
	if _, err := db.Exec(ctx, `ALTER TABLE model_prices ENABLE TRIGGER model_prices_immutable_trigger`); err != nil {
		return err
	}
	return deleteErr
}

// insertTestUsage writes a row for a request that was served: 200 with
// error_class=ok. B4.3 keys its policy off the outcome as well as the usage
// source, so the outcome has to be a real value here and not a placeholder.
func insertTestUsage(ctx context.Context, t *testing.T, db *pgxpool.Pool, tenantID, eventID, model, usageSource string, partial bool, occurredAt time.Time, tokens [4]int64) {
	t.Helper()
	insertTestUsageWithOutcome(ctx, t, db, tenantID, eventID, model, usageSource, partial, occurredAt, tokens, 200, string(contracts.ErrorOK))
}

func insertTestUsageWithOutcome(ctx context.Context, t *testing.T, db *pgxpool.Pool, tenantID, eventID, model, usageSource string, partial bool, occurredAt time.Time, tokens [4]int64, statusCode int, errorClass string) {
	t.Helper()
	if _, err := db.Exec(ctx, `
		INSERT INTO usage_ledger (event_id,attempt_id,request_id,tenant_id,account_id,provider,model,
			status_code,error_class,tokens_in,tokens_out,cache_read_tokens,cache_write_tokens,
			usage_source,partial,occurred_at)
		VALUES ($1,$1||'-attempt',$1||'-request',$2,'b42-account','b42-provider',$3,$11,$12,
			$4,$5,$6,$7,$8,$9,$10)`,
		eventID, tenantID, model, tokens[0], tokens[1], tokens[2], tokens[3], usageSource, partial, occurredAt,
		statusCode, errorClass); err != nil {
		t.Fatal(err)
	}
}

func TestBillingJobSettlesLedgerRowsAndWalletInOneTransaction(t *testing.T) {
	ctx, db := openBillingTestDB(t)
	const tenantID = "b42-tenant"
	const provider = "b42-provider"
	repo := PGBillingRepository{DB: db}
	job := BillingJob{DB: db}

	if err := purgeAllTestPrices(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM usage_ledger WHERE tenant_id=$1`, tenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM tenants WHERE id=$1`, tenantID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		background := context.Background()
		db.Exec(background, `DELETE FROM usage_ledger WHERE tenant_id=$1`, tenantID)
		db.Exec(background, `DELETE FROM tenants WHERE id=$1`, tenantID)
		purgeAllTestPrices(background, db)
	})

	now := time.Now().UTC()
	windowEnd := now

	// Deployment-order guard: with no prices loaded the job must do nothing
	// rather than settle every row as permanently `unpriced`.
	skipped, err := job.RunOnce(ctx, windowEnd)
	if err != nil {
		t.Fatal(err)
	}
	if !skipped.Skipped || skipped.RowsBilled != 0 || skipped.RowsUnpriced != 0 {
		t.Fatalf("run with an empty price table = %+v", skipped)
	}
	var skippedStatus string
	if err := db.QueryRow(ctx, `SELECT status FROM billing_runs WHERE id=$1`, skipped.RunID).Scan(&skippedStatus); err != nil {
		t.Fatal(err)
	}
	if skippedStatus != "skipped" {
		t.Errorf("skipped run recorded as %q", skippedStatus)
	}

	// InsertModelPrice takes the reference instant as an argument precisely so
	// a caller can build a historical fixture; here it lets the price be in
	// effect before the ledger rows without weakening the forward-only rule.
	reference := now.Add(-24 * time.Hour)
	price := ModelPrice{
		ID: "b42-price", Provider: provider, Model: "b42-model", Currency: "USD", UnitScale: 1000000,
		PriceInput: "3", PriceOutput: "15", PriceCacheRead: "0.3", PriceCacheWrite: "3.75",
		EffectiveFrom: now.Add(-12 * time.Hour),
	}
	if err := repo.InsertModelPrice(ctx, price, reference); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(ctx, `INSERT INTO tenants (id,name) VALUES ($1,'B4.2 billing fixture')`, tenantID); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureWallet(ctx, tenantID, "USD"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ApplyMovement(ctx, WalletMovement{ID: "b42-topup", TenantID: tenantID, Kind: "topup", Amount: "10"}); err != nil {
		t.Fatal(err)
	}

	billable := now.Add(-time.Hour)
	insertTestUsage(ctx, t, db, tenantID, "b42-priced", "b42-model", "upstream", false, billable, [4]int64{1000, 500, 2000, 400})
	insertTestUsage(ctx, t, db, tenantID, "b42-zero", "b42-model", "upstream", false, billable, [4]int64{0, 0, 0, 0})
	insertTestUsage(ctx, t, db, tenantID, "b42-unpriced", "b42-other-model", "upstream", false, billable, [4]int64{100, 100, 0, 0})
	insertTestUsage(ctx, t, db, tenantID, "b42-estimated", "b42-model", "estimated", false, billable, [4]int64{9999, 9999, 0, 0})
	// A truncated stream that still carried upstream usage: charged in full.
	insertTestUsage(ctx, t, db, tenantID, "b42-partial", "b42-model", "upstream", true, billable, [4]int64{9999, 9999, 0, 0})
	// Served, but the token counts were lost: held for a human, never zeroed.
	insertTestUsage(ctx, t, db, tenantID, "b42-missing-served", "b42-model", "missing", false, billable, [4]int64{0, 0, 0, 0})
	// Upstream reported usage on a request that failed: deliberately free.
	insertTestUsageWithOutcome(ctx, t, db, tenantID, "b42-failed", "b42-model", "upstream", false, billable,
		[4]int64{7000, 3000, 0, 0}, 500, string(contracts.ErrorUpstream5xx))
	insertTestUsage(ctx, t, db, tenantID, "b42-future", "b42-model", "upstream", false, now.Add(time.Hour), [4]int64{9999, 9999, 0, 0})

	run, err := job.RunOnce(ctx, windowEnd)
	if err != nil {
		t.Fatal(err)
	}
	// b42-priced: (1000*3 + 500*15 + 2000*0.3 + 400*3.75) / 1e6 = 0.0126
	// b42-partial: 9999*(3+15) / 1e6                            = 0.179982
	const wantDebited = contracts.Decimal("0.192582000000")
	if run.Skipped || run.RowsBilled != 3 || run.RowsUnpriced != 1 || run.TenantsFailed != 0 {
		t.Fatalf("run = %+v", run)
	}
	if run.RowsHeld != 2 || run.RowsNotBillable != 1 || run.RowsUnknownClass != 0 {
		t.Fatalf("B4.3 buckets = held %d, not billable %d, unknown %d", run.RowsHeld, run.RowsNotBillable, run.RowsUnknownClass)
	}
	if run.TotalDebited != wantDebited {
		t.Fatalf("total debited = %q, want %q", run.TotalDebited, wantDebited)
	}

	states := map[string]string{}
	amounts := map[string]*string{}
	rows, err := db.Query(ctx, `SELECT event_id,billing_state,billed_amount::text FROM usage_ledger WHERE tenant_id=$1`, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var eventID, state string
		var amount *string
		if err := rows.Scan(&eventID, &state, &amount); err != nil {
			t.Fatal(err)
		}
		states[eventID] = state
		amounts[eventID] = amount
	}
	rows.Close()
	want := map[string]string{
		"b42-priced":         "billed",
		"b42-zero":           "billed",
		"b42-partial":        "billed",
		"b42-unpriced":       "unpriced",
		"b42-estimated":      "held",
		"b42-missing-served": "held",
		"b42-failed":         "not_billable",
		"b42-future":         "pending", // outside the window, deferred not lost
	}
	for eventID, wantState := range want {
		if states[eventID] != wantState {
			t.Errorf("%s billing_state = %q, want %q", eventID, states[eventID], wantState)
		}
	}
	if amounts["b42-priced"] == nil || *amounts["b42-priced"] != "0.012600000000" {
		t.Errorf("billed_amount = %v", amounts["b42-priced"])
	}
	if amounts["b42-zero"] == nil || *amounts["b42-zero"] != "0.000000000000" {
		t.Errorf("a zero-cost row must be settled at 0, got %v", amounts["b42-zero"])
	}
	// An unpriced row must not carry an amount: it was not charged. Neither
	// must a held or not-billable one -- a 0 there would be indistinguishable
	// from a row that genuinely cost nothing.
	for _, eventID := range []string{"b42-unpriced", "b42-estimated", "b42-missing-served", "b42-failed"} {
		if amounts[eventID] != nil {
			t.Errorf("%s was not charged but carries billed_amount %v", eventID, *amounts[eventID])
		}
	}

	balance, _, err := repo.Balance(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if balance != contracts.Decimal("9.807418000000") {
		t.Fatalf("balance = %q, want 9.807418000000", balance)
	}

	// Exactly one debit, and the wallet debit equals the sum of the
	// billed_amount column -- the identity B4.5 will reconcile against.
	var debits int
	var debited string
	if err := db.QueryRow(ctx, `SELECT count(*),COALESCE(sum(amount),0)::text FROM wallet_transactions WHERE tenant_id=$1 AND kind='debit' AND billing_run_id=$2`, tenantID, run.RunID).Scan(&debits, &debited); err != nil {
		t.Fatal(err)
	}
	if debits != 1 || debited != "-"+string(wantDebited) {
		t.Fatalf("debits = %d totalling %s", debits, debited)
	}
	var ledgerSum string
	if err := db.QueryRow(ctx, `SELECT COALESCE(sum(billed_amount),0)::text FROM usage_ledger WHERE tenant_id=$1 AND billing_state='billed'`, tenantID).Scan(&ledgerSum); err != nil {
		t.Fatal(err)
	}
	if ledgerSum != string(wantDebited) {
		t.Errorf("ledger billed sum %s != wallet debit", ledgerSum)
	}

	var status string
	var rowsBilled, rowsUnpriced, rowsHeld, rowsNotBillable int
	var runTotal string
	if err := db.QueryRow(ctx, `SELECT status,rows_billed,rows_unpriced,rows_held,rows_not_billable,total_debited::text FROM billing_runs WHERE id=$1`, run.RunID).
		Scan(&status, &rowsBilled, &rowsUnpriced, &rowsHeld, &rowsNotBillable, &runTotal); err != nil {
		t.Fatal(err)
	}
	if status != "completed" || rowsBilled != 3 || rowsUnpriced != 1 || runTotal != string(wantDebited) {
		t.Errorf("billing_runs row = %s %d/%d %s", status, rowsBilled, rowsUnpriced, runTotal)
	}
	// The buckets are persisted separately: a run that held everything and
	// billed nothing must not be readable as a healthy run.
	if rowsHeld != 2 || rowsNotBillable != 1 {
		t.Errorf("billing_runs buckets = held %d, not billable %d; want 2 and 1", rowsHeld, rowsNotBillable)
	}

	// Re-running must not charge anything again, and must not re-decide the
	// held or not-billable rows either: all four states are terminal.
	second, err := job.RunOnce(ctx, windowEnd)
	if err != nil {
		t.Fatal(err)
	}
	if second.RowsBilled != 0 || second.RowsUnpriced != 0 || second.RowsHeld != 0 || second.RowsNotBillable != 0 {
		t.Fatalf("re-run settled rows again: %+v", second)
	}
	after, _, err := repo.Balance(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if after != balance {
		t.Fatalf("re-run changed the balance from %q to %q", balance, after)
	}
	var totalDebits int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM wallet_transactions WHERE tenant_id=$1 AND kind='debit'`, tenantID).Scan(&totalDebits); err != nil {
		t.Fatal(err)
	}
	if totalDebits != 1 {
		t.Fatalf("wallet has %d debits after two runs", totalDebits)
	}
	db.Exec(ctx, `DELETE FROM billing_runs WHERE id = ANY($1)`, []string{skipped.RunID, run.RunID, second.RunID})
}

func TestBillingJobLeavesRowsPendingWhenATenantHasNoWallet(t *testing.T) {
	ctx, db := openBillingTestDB(t)
	const tenantID = "b42-walletless-tenant"
	const provider = "b42-provider"
	repo := PGBillingRepository{DB: db}
	job := BillingJob{DB: db}

	if err := purgeAllTestPrices(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM usage_ledger WHERE tenant_id=$1`, tenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM tenants WHERE id=$1`, tenantID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		background := context.Background()
		db.Exec(background, `DELETE FROM usage_ledger WHERE tenant_id=$1`, tenantID)
		db.Exec(background, `DELETE FROM tenants WHERE id=$1`, tenantID)
		purgeAllTestPrices(background, db)
	})

	now := time.Now().UTC()
	if err := repo.InsertModelPrice(ctx, ModelPrice{
		ID: "b42-walletless-price", Provider: provider, Model: "b42-model", UnitScale: 1000,
		PriceInput: "1", PriceOutput: "1", PriceCacheRead: "0", PriceCacheWrite: "0",
		EffectiveFrom: now.Add(-12 * time.Hour),
	}, now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO tenants (id,name) VALUES ($1,'B4.2 walletless fixture')`, tenantID); err != nil {
		t.Fatal(err)
	}
	insertTestUsage(ctx, t, db, tenantID, "b42-walletless-usage", "b42-model", "upstream", false, now.Add(-time.Hour), [4]int64{1000, 0, 0, 0})

	run, err := job.RunOnce(ctx, now)
	// Usage with no wallet to charge is an operator error, not a free ride:
	// it must surface, and the rows must stay pending for a retry.
	if err == nil {
		t.Fatal("a tenant with usage but no wallet was settled silently")
	}
	if run.TenantsFailed != 1 || run.RowsBilled != 0 {
		t.Fatalf("run = %+v", run)
	}
	var state string
	if err := db.QueryRow(ctx, `SELECT billing_state FROM usage_ledger WHERE event_id=$1`, "b42-walletless-usage").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "pending" {
		t.Fatalf("billing_state = %q, want pending so the row is retried", state)
	}
	var status string
	if err := db.QueryRow(ctx, `SELECT status FROM billing_runs WHERE id=$1`, run.RunID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Errorf("run status = %q, want failed", status)
	}

	// Once the wallet exists the same rows settle on the next run.
	if err := repo.EnsureWallet(ctx, tenantID, "USD"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ApplyMovement(ctx, WalletMovement{ID: "b42-walletless-topup", TenantID: tenantID, Kind: "topup", Amount: "5"}); err != nil {
		t.Fatal(err)
	}
	retry, err := job.RunOnce(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if retry.RowsBilled != 1 || retry.TenantsFailed != 0 {
		t.Fatalf("retry = %+v", retry)
	}
	balance, _, err := repo.Balance(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if balance != contracts.Decimal("4.000000000000") {
		t.Fatalf("balance = %q, want 4.000000000000 (1000 tokens at 1 per 1000)", balance)
	}
	db.Exec(ctx, `DELETE FROM billing_runs WHERE id = ANY($1)`, []string{run.RunID, retry.RunID})
}
