package control

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The reconciliation fixtures live in 2021 on purpose. The gated suite shares
// one PostgreSQL database, and every other billing test seeds rows around
// `now`; an ancient window guarantees this report sees this test's rows and
// nothing else, so an assertion about the whole window is still an assertion
// about this test.
var (
	b45WindowStart = time.Date(2021, 5, 4, 0, 0, 0, 0, time.UTC)
	b45WindowEnd   = time.Date(2021, 5, 5, 0, 0, 0, 0, time.UTC)
	b45OccurredAt  = time.Date(2021, 5, 4, 9, 15, 0, 0, time.UTC)
)

// b45Fixture seeds one tenant with a funded wallet, a price, and four ledger
// rows covering the outcomes B4.3 produces: two billable, one that cannot be
// priced, one that has to be held.
func b45Fixture(t *testing.T, tenantID string) (context.Context, *pgxpool.Pool) {
	t.Helper()
	ctx, db := openBillingTestDB(t)
	cleanup := func() {
		background := context.Background()
		db.Exec(background, `DELETE FROM usage_ledger WHERE tenant_id=$1`, tenantID)
		db.Exec(background, `DELETE FROM wallet_transactions WHERE tenant_id=$1`, tenantID)
		db.Exec(background, `DELETE FROM tenant_wallets WHERE tenant_id=$1`, tenantID)
		db.Exec(background, `DELETE FROM tenants WHERE id=$1`, tenantID)
		db.Exec(background, `ALTER TABLE model_prices DISABLE TRIGGER model_prices_immutable_trigger`)
		db.Exec(background, `DELETE FROM model_prices WHERE id LIKE 'b45-%'`)
		db.Exec(background, `ALTER TABLE model_prices ENABLE TRIGGER model_prices_immutable_trigger`)
	}
	cleanup()
	t.Cleanup(cleanup)

	repo := PGBillingRepository{DB: db}
	if err := repo.InsertModelPrice(ctx, ModelPrice{
		ID: "b45-price-" + tenantID, Provider: "b42-provider", Model: "b45-model", Currency: "USD", UnitScale: 1000000,
		PriceInput: "3", PriceOutput: "15", PriceCacheRead: "0.3", PriceCacheWrite: "3.75",
		EffectiveFrom: b45WindowStart.Add(-72 * time.Hour),
	}, b45WindowStart.Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO tenants (id,name) VALUES ($1,'B4.5 reconciliation fixture')`, tenantID); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureWallet(ctx, tenantID, "USD"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ApplyMovement(ctx, WalletMovement{ID: "b45-topup-" + tenantID, TenantID: tenantID, Kind: "topup", Amount: "10"}); err != nil {
		t.Fatal(err)
	}

	// 3*1000/1e6 + 15*500/1e6 = 0.0105
	insertTestUsage(ctx, t, db, tenantID, "b45-billed-a-"+tenantID, "b45-model", "upstream", false, b45OccurredAt, [4]int64{1000, 500, 0, 0})
	// 3*2000/1e6 = 0.006
	insertTestUsage(ctx, t, db, tenantID, "b45-billed-b-"+tenantID, "b45-model", "upstream", false, b45OccurredAt, [4]int64{2000, 0, 0, 0})
	// No price was ever in effect for this model: revenue we cannot collect.
	insertTestUsage(ctx, t, db, tenantID, "b45-unpriced-"+tenantID, "b45-other-model", "upstream", false, b45OccurredAt, [4]int64{100, 100, 0, 0})
	// Served, but the token counts were lost: held for a human.
	insertTestUsage(ctx, t, db, tenantID, "b45-held-"+tenantID, "b45-model", "missing", false, b45OccurredAt, [4]int64{0, 0, 0, 0})
	return ctx, db
}

// The B4.5 DoD: over a window, ledger rows, the amount debited and the wallet
// movement all line up, and everything that did not turn into revenue is
// listed by event_id rather than netted away.
func TestReconciliationAlignsLedgerDebitsAndWalletOverAWindow(t *testing.T) {
	const tenantID = "b45-tenant"
	ctx, db := b45Fixture(t, tenantID)

	balanceBefore, _, err := (PGBillingRepository{DB: db}).Balance(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}

	run, err := (BillingJob{DB: db}).RunOnce(ctx, b45WindowEnd)
	if err != nil {
		t.Fatal(err)
	}
	if run.Skipped {
		t.Fatalf("billing run was skipped: %s", run.SkipReason)
	}

	report, err := (Reconciliation{DB: db}).Run(ctx, b45WindowStart, b45WindowEnd)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Balanced() {
		t.Fatalf("a freshly settled window did not reconcile: %+v", report.Discrepancies)
	}
	if len(report.Tenants) != 1 || report.Tenants[0].TenantID != tenantID {
		t.Fatalf("window covered %d tenants, want only %s: %+v", len(report.Tenants), tenantID, report.Tenants)
	}
	tenant := report.Tenants[0]

	// Side one: the ledger. Two rows billed, one unpriced, one held.
	want := map[string]int{"billed": 2, "unpriced": 1, "held": 1}
	for state, count := range want {
		if tenant.RowsByState[state] != count {
			t.Errorf("rows in state %s = %d, want %d (all: %v)", state, tenant.RowsByState[state], count, tenant.RowsByState)
		}
	}
	if !sameMoney(tenant.BilledAmount, "0.0165") {
		t.Errorf("billed amount = %s, want 0.0165", tenant.BilledAmount)
	}

	// Side two: the debit. Equal to the ledger sum, to the last digit.
	if !sameMoney(report.TotalDebited, report.TotalBilled) {
		t.Errorf("debited %s but billed %s", report.TotalDebited, report.TotalBilled)
	}
	if len(tenant.Runs) != 1 {
		t.Fatalf("window rows were settled by %d runs, want 1: %+v", len(tenant.Runs), tenant.Runs)
	}
	if tenant.Runs[0].RunID != run.RunID {
		t.Errorf("window rows point at run %s, want %s", tenant.Runs[0].RunID, run.RunID)
	}
	if sign, _ := tenant.Runs[0].Difference.Sign(); sign != 0 {
		t.Errorf("run difference = %s, want zero", tenant.Runs[0].Difference)
	}

	// Side three: the wallet. The balance moved by exactly what was debited,
	// and the balance is still the sum of its own journal.
	moved, ok := balanceBefore.Sub(tenant.WalletBalance)
	if !ok {
		t.Fatalf("cannot subtract %q from %q", tenant.WalletBalance, balanceBefore)
	}
	if !sameMoney(moved, report.TotalDebited) {
		t.Errorf("wallet moved by %s but the run debited %s", moved, report.TotalDebited)
	}
	if !tenant.WalletExists || !sameMoney(tenant.WalletBalance, tenant.JournalSum) {
		t.Errorf("balance %s does not match the journal sum %s", tenant.WalletBalance, tenant.JournalSum)
	}

	// The gap: rows the window contains but revenue does not, each named. A
	// window where everything was held would otherwise balance perfectly
	// while earning nothing.
	unsettled := map[string]string{}
	for _, row := range report.UnsettledRows {
		unsettled[row.EventID] = row.State
	}
	if got := unsettled["b45-unpriced-"+tenantID]; got != "unpriced" {
		t.Errorf("unpriced row listed as %q", got)
	}
	if got := unsettled["b45-held-"+tenantID]; got != "held" {
		t.Errorf("held row listed as %q", got)
	}
	if len(unsettled) != 2 {
		t.Errorf("unsettled rows = %v, want exactly the unpriced and held ones", report.UnsettledRows)
	}

	// Re-running the report changes nothing: it is a read, not a step in the
	// pipeline.
	again, err := (Reconciliation{DB: db}).Run(ctx, b45WindowStart, b45WindowEnd)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Balanced() || !sameMoney(again.TotalBilled, report.TotalBilled) {
		t.Errorf("a second reconciliation disagreed with the first: %+v", again.Discrepancies)
	}
}

// A difference is only useful if it names the request that caused it.
func TestReconciliationAttributesDifferencesToEventIDs(t *testing.T) {
	const tenantID = "b45-drift-tenant"
	ctx, db := b45Fixture(t, tenantID)

	if _, err := (BillingJob{DB: db}).RunOnce(ctx, b45WindowEnd); err != nil {
		t.Fatal(err)
	}
	tampered := "b45-billed-a-" + tenantID
	// Simulate the failure the reconciliation exists to catch: the ledger says
	// one row was worth more than the wallet was ever asked to pay.
	if _, err := db.Exec(ctx, `UPDATE usage_ledger SET billed_amount = billed_amount + 1 WHERE event_id=$1`, tampered); err != nil {
		t.Fatal(err)
	}
	// And a balance that moved without a journal entry.
	if _, err := db.Exec(ctx, `UPDATE tenant_wallets SET balance = balance + 5 WHERE tenant_id=$1`, tenantID); err != nil {
		t.Fatal(err)
	}

	report, err := (Reconciliation{DB: db}).Run(ctx, b45WindowStart, b45WindowEnd)
	if err != nil {
		t.Fatal(err)
	}
	if report.Balanced() {
		t.Fatal("a tampered ledger and a drifting balance both reconciled clean")
	}

	byKind := map[string]Discrepancy{}
	for _, discrepancy := range report.Discrepancies {
		byKind[discrepancy.Kind] = discrepancy
	}
	mismatch, ok := byKind[DiscrepancyRunDebitMismatch]
	if !ok {
		t.Fatalf("no run/debit mismatch reported: %+v", report.Discrepancies)
	}
	if difference, _ := mismatch.Expected.Sub(mismatch.Actual); !sameMoney(difference, "1") {
		t.Errorf("mismatch difference = %s (ledger %s, debit %s), want 1", difference, mismatch.Expected, mismatch.Actual)
	}
	// The DoD's actual requirement: the difference has to be chaseable to a
	// specific request.
	found := false
	for _, eventID := range mismatch.EventIDs {
		if eventID == tampered {
			found = true
		}
	}
	if !found {
		t.Errorf("mismatch does not name the tampered event %s: %v", tampered, mismatch.EventIDs)
	}

	drift, ok := byKind[DiscrepancyWalletBalanceDrift]
	if !ok {
		t.Fatalf("a balance moved without a journal entry went unreported: %+v", report.Discrepancies)
	}
	if difference, _ := drift.Actual.Sub(drift.Expected); !sameMoney(difference, "5") {
		t.Errorf("balance drift = %s (balance %s, journal %s), want 5", difference, drift.Actual, drift.Expected)
	}
}

// "对账本身只读，不修数据" has to hold for code paths no test exercises, so it
// is asserted against the source rather than against one run's side effects.
// The read-only transaction in Run is the enforcement; this is the tripwire
// that fires before anyone discovers PostgreSQL refusing a write in
// production.
func TestReconciliationSourceContainsNoWrites(t *testing.T) {
	const path = "billing_reconcile.go"
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), path, source, parser.SkipObjectResolution); err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, statement := range []string{"INSERT ", "UPDATE ", "DELETE ", "ALTER ", "TRUNCATE ", ".Exec("} {
		if strings.Contains(text, statement) {
			t.Errorf("%s contains %q: the reconciliation must never write", path, strings.TrimSpace(statement))
		}
	}
	// Guard against the check passing because the file moved or emptied.
	if !strings.Contains(text, "pgx.ReadOnly") {
		t.Errorf("%s no longer opens a read-only transaction", path)
	}
}
