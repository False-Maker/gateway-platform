package control

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Reconciliation aligns three independently written records of the same money
// over a time window:
//
//	usage_ledger        -- what we decided each request was worth
//	wallet_transactions -- what we actually took off the tenant
//	tenant_wallets      -- what the tenant's balance says is left
//
// It is strictly read-only. That is not a convention here: every query runs in
// a PostgreSQL read-only transaction, so a stray write in this file would be
// refused by the database rather than silently "fixing" a difference. A
// reconciliation that repairs its own input cannot be trusted to detect the
// next problem -- the whole point is to be an independent witness.
type Reconciliation struct{ DB *pgxpool.Pool }

// The window is over usage_ledger.occurred_at -- when the usage happened --
// not over when it was billed. Those are different clocks: a run settles rows
// minutes after they occurred, so aligning wallet movements to the same wall
// clock would report a difference on every boundary. Instead, window rows are
// followed to their billing_run_id and compared against that run's debit.
type ReconciliationReport struct {
	Start       time.Time
	End         time.Time
	GeneratedAt time.Time

	Tenants []TenantReconciliation

	// Rows the window contains but revenue does not. B4-BILLING-MODEL's B4.5
	// addendum requires these to be listed rather than netted away: a window
	// where every row was held would otherwise "balance" perfectly while
	// earning nothing.
	UnsettledRows []UnsettledRow

	TotalBilled  contracts.Decimal
	TotalDebited contracts.Decimal

	Discrepancies []Discrepancy
}

// Balanced is true when all three records agree and nothing is unexplained.
// Unsettled rows do not make a window unbalanced -- they are accounted for,
// just not as revenue.
func (r ReconciliationReport) Balanced() bool { return len(r.Discrepancies) == 0 }

type TenantReconciliation struct {
	TenantID string
	// Row counts by billing_state within the window.
	RowsByState map[string]int
	// Sum of billed_amount for the window's `billed` rows.
	BilledAmount contracts.Decimal
	// The runs that settled this tenant's window rows, each compared against
	// its own wallet debit.
	Runs []RunReconciliation
	// WalletExists distinguishes "balance is zero" from "there is no wallet".
	WalletExists  bool
	WalletBalance contracts.Decimal
	// JournalSum is the sum of every wallet_transactions row for the tenant,
	// over all time. It is deliberately not windowed: the balance is a running
	// total since the wallet was created, so only the full journal can prove
	// the balance was not moved out of band.
	JournalSum contracts.Decimal
}

// RunReconciliation compares one billing run's ledger footprint against the
// single wallet debit it wrote. Both sides cover the run's *entire* footprint,
// not just the part inside the window: a run settles rows by occurred_at <
// window_end, so clipping one side to the report window would manufacture a
// difference that is really just a boundary.
type RunReconciliation struct {
	RunID string
	// Rows of this run that fall inside the report window, and their event
	// ids -- this is what makes a difference explainable to a specific event.
	WindowRows     int
	WindowEventIDs []string
	// The run's full footprint.
	LedgerRows   int
	LedgerAmount contracts.Decimal
	WalletDebit  contracts.Decimal
	Difference   contracts.Decimal
}

// UnsettledRow is one window row that produced no revenue, named by event id
// so the gap can be chased to a specific request.
type UnsettledRow struct {
	TenantID string
	EventID  string
	State    string
}

// Discrepancy is one thing the three records disagree about. EventIDs names
// the rows involved whenever the difference can be attributed to specific
// requests, which the B4.5 DoD requires.
type Discrepancy struct {
	Kind     string
	TenantID string
	RunID    string
	EventIDs []string
	Expected contracts.Decimal
	Actual   contracts.Decimal
	Detail   string
}

const (
	// A billed row that carries no amount: the ledger says we charged for it
	// but cannot say how much, so the wallet debit can never be justified.
	DiscrepancyBilledWithoutAmount = "billed_row_without_amount"
	// A billed row with no run: nothing links it to a wallet movement.
	DiscrepancyBilledWithoutRun = "billed_row_without_run"
	// An amount on a row that was not billed. Under B4.3 `held` and
	// `not_billable` rows carry NULL, precisely so a 0 cannot be confused
	// with a settled row that genuinely cost nothing.
	DiscrepancyAmountOnUnsettledRow = "amount_on_unsettled_row"
	// The run's ledger sum and its wallet debit differ.
	DiscrepancyRunDebitMismatch = "run_debit_mismatch"
	// The balance is not the sum of the journal, so something wrote the
	// balance without a journal entry.
	DiscrepancyWalletBalanceDrift = "wallet_balance_drift"
	// Rows were billed against a tenant that has no wallet at all.
	DiscrepancyMissingWallet = "missing_wallet"
)

type windowRow struct {
	tenantID string
	eventID  string
	state    string
	amount   string
	hasAmt   bool
	runID    string
}

// Run produces the report for [start, end). It never writes.
func (r Reconciliation) Run(ctx context.Context, start, end time.Time) (ReconciliationReport, error) {
	if r.DB == nil {
		return ReconciliationReport{}, errors.New("reconciliation database pool is nil")
	}
	start, end = start.UTC(), end.UTC()
	if !end.After(start) {
		return ReconciliationReport{}, fmt.Errorf("reconciliation window end %s is not after start %s",
			end.Format(time.RFC3339Nano), start.Format(time.RFC3339Nano))
	}
	report := ReconciliationReport{Start: start, End: end, GeneratedAt: time.Now().UTC(),
		TotalBilled: zeroMoney(), TotalDebited: zeroMoney()}

	// One read-only snapshot for the whole report: the three sides must be
	// read as they were at a single instant, or a billing run committing
	// mid-report would show up as a difference that does not exist.
	tx, err := r.DB.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return report, fmt.Errorf("begin read-only reconciliation: %w", err)
	}
	defer tx.Rollback(ctx)

	rows, err := windowRows(ctx, tx, start, end)
	if err != nil {
		return report, err
	}

	byTenant := map[string]*TenantReconciliation{}
	// The window's own billed total, accumulated per tenant as the rows are
	// read. It is summed from the window rows rather than from the runs,
	// because a run may settle rows on both sides of the window boundary.
	windowBilled := map[string]*big.Rat{}
	runRows := map[string]map[string]*RunReconciliation{}
	var runIDs []string
	seenRun := map[string]bool{}
	for _, row := range rows {
		tenant := byTenant[row.tenantID]
		if tenant == nil {
			tenant = &TenantReconciliation{TenantID: row.tenantID, RowsByState: map[string]int{},
				BilledAmount: zeroMoney(), WalletBalance: zeroMoney(), JournalSum: zeroMoney()}
			byTenant[row.tenantID] = tenant
			windowBilled[row.tenantID] = new(big.Rat)
		}
		tenant.RowsByState[row.state]++

		if row.state != "billed" {
			report.UnsettledRows = append(report.UnsettledRows,
				UnsettledRow{TenantID: row.tenantID, EventID: row.eventID, State: row.state})
			if row.hasAmt {
				report.Discrepancies = append(report.Discrepancies, Discrepancy{
					Kind: DiscrepancyAmountOnUnsettledRow, TenantID: row.tenantID,
					EventIDs: []string{row.eventID}, Actual: contracts.Decimal(row.amount),
					Detail: fmt.Sprintf("state %s carries an amount; only billed rows may", row.state),
				})
			}
			continue
		}
		if !row.hasAmt {
			report.Discrepancies = append(report.Discrepancies, Discrepancy{
				Kind: DiscrepancyBilledWithoutAmount, TenantID: row.tenantID, RunID: row.runID,
				EventIDs: []string{row.eventID}, Detail: "billed row has a NULL billed_amount",
			})
			continue
		}
		amount, ok := new(big.Rat).SetString(row.amount)
		if !ok {
			return report, fmt.Errorf("event %s has an unparseable billed_amount %q", row.eventID, row.amount)
		}
		windowBilled[row.tenantID].Add(windowBilled[row.tenantID], amount)
		if row.runID == "" {
			report.Discrepancies = append(report.Discrepancies, Discrepancy{
				Kind: DiscrepancyBilledWithoutRun, TenantID: row.tenantID,
				EventIDs: []string{row.eventID}, Actual: contracts.Decimal(row.amount),
				Detail: "billed row has no billing_run_id, so no wallet movement can be matched to it",
			})
			continue
		}
		if !seenRun[row.runID] {
			seenRun[row.runID] = true
			runIDs = append(runIDs, row.runID)
		}
		if runRows[row.tenantID] == nil {
			runRows[row.tenantID] = map[string]*RunReconciliation{}
		}
		run := runRows[row.tenantID][row.runID]
		if run == nil {
			run = &RunReconciliation{RunID: row.runID, LedgerAmount: zeroMoney(),
				WalletDebit: zeroMoney(), Difference: zeroMoney()}
			runRows[row.tenantID][row.runID] = run
		}
		run.WindowRows++
		run.WindowEventIDs = append(run.WindowEventIDs, row.eventID)
	}

	ledgerByRun, err := runLedgerTotals(ctx, tx, runIDs)
	if err != nil {
		return report, err
	}
	debitByRun, err := runWalletDebits(ctx, tx, runIDs)
	if err != nil {
		return report, err
	}

	tenantIDs := make([]string, 0, len(byTenant))
	for id := range byTenant {
		tenantIDs = append(tenantIDs, id)
	}
	sort.Strings(tenantIDs)
	wallets, err := walletTotals(ctx, tx, tenantIDs)
	if err != nil {
		return report, err
	}

	billedTotal, debitedTotal := new(big.Rat), new(big.Rat)
	for _, tenantID := range tenantIDs {
		tenant := byTenant[tenantID]
		wallet, ok := wallets[tenantID]
		tenant.WalletExists = ok
		if ok {
			tenant.WalletBalance = wallet.balance
			tenant.JournalSum = wallet.journalSum
			if !sameMoney(wallet.balance, wallet.journalSum) {
				difference, _ := contracts.Decimal(wallet.balance).Sub(wallet.journalSum)
				report.Discrepancies = append(report.Discrepancies, Discrepancy{
					Kind: DiscrepancyWalletBalanceDrift, TenantID: tenantID,
					Expected: wallet.journalSum, Actual: wallet.balance,
					Detail: fmt.Sprintf("balance differs from the journal sum by %s; the balance moved without a wallet_transactions row", difference),
				})
			}
		}

		runKeys := make([]string, 0, len(runRows[tenantID]))
		for runID := range runRows[tenantID] {
			runKeys = append(runKeys, runID)
		}
		sort.Strings(runKeys)
		for _, runID := range runKeys {
			run := runRows[tenantID][runID]
			key := runKey{tenantID: tenantID, runID: runID}
			if total, ok := ledgerByRun[key]; ok {
				run.LedgerRows = total.rows
				run.LedgerAmount = total.amount
			}
			if debit, ok := debitByRun[key]; ok {
				run.WalletDebit = debit
			}
			difference, ok := contracts.Decimal(run.LedgerAmount).Sub(run.WalletDebit)
			if !ok {
				return report, fmt.Errorf("run %s tenant %s: cannot subtract %q from %q",
					runID, tenantID, run.WalletDebit, run.LedgerAmount)
			}
			run.Difference = difference
			if sign, _ := difference.Sign(); sign != 0 {
				if !tenant.WalletExists {
					report.Discrepancies = append(report.Discrepancies, Discrepancy{
						Kind: DiscrepancyMissingWallet, TenantID: tenantID, RunID: runID,
						EventIDs: run.WindowEventIDs, Expected: run.LedgerAmount, Actual: zeroMoney(),
						Detail: "the tenant was billed but has no wallet row",
					})
				} else {
					report.Discrepancies = append(report.Discrepancies, Discrepancy{
						Kind: DiscrepancyRunDebitMismatch, TenantID: tenantID, RunID: runID,
						EventIDs: run.WindowEventIDs, Expected: run.LedgerAmount, Actual: run.WalletDebit,
						Detail: fmt.Sprintf("run settled %d ledger rows worth %s but debited %s (difference %s); the window rows of this run are listed above",
							run.LedgerRows, run.LedgerAmount, run.WalletDebit, difference),
					})
				}
			}
			tenant.Runs = append(tenant.Runs, *run)
			debit, ok := new(big.Rat).SetString(string(run.WalletDebit))
			if !ok {
				return report, fmt.Errorf("run %s wallet debit %q is not a decimal", runID, run.WalletDebit)
			}
			debitedTotal.Add(debitedTotal, debit)
		}

		// The window's own billed total is summed from the window rows, not
		// from the runs, because a run may reach outside the window.
		tenantBilled := windowBilled[tenantID]
		tenant.BilledAmount = contracts.Decimal(tenantBilled.FloatString(moneyScale))
		billedTotal.Add(billedTotal, tenantBilled)
		report.Tenants = append(report.Tenants, *tenant)
	}
	report.TotalBilled = contracts.Decimal(billedTotal.FloatString(moneyScale))
	report.TotalDebited = contracts.Decimal(debitedTotal.FloatString(moneyScale))
	return report, nil
}

func windowRows(ctx context.Context, q pgQuerier, start, end time.Time) ([]windowRow, error) {
	rows, err := q.Query(ctx, `
		SELECT tenant_id, event_id, billing_state,
		       billed_amount::text, COALESCE(billing_run_id,'')
		FROM usage_ledger
		WHERE occurred_at >= $1 AND occurred_at < $2
		ORDER BY tenant_id, occurred_at, id`, start, end)
	if err != nil {
		return nil, fmt.Errorf("read usage ledger window: %w", err)
	}
	defer rows.Close()
	var window []windowRow
	for rows.Next() {
		var row windowRow
		var amount *string
		if err := rows.Scan(&row.tenantID, &row.eventID, &row.state, &amount, &row.runID); err != nil {
			return nil, err
		}
		if amount != nil {
			row.amount, row.hasAmt = *amount, true
		}
		window = append(window, row)
	}
	return window, rows.Err()
}

type runKey struct{ tenantID, runID string }

type runLedgerTotal struct {
	rows   int
	amount contracts.Decimal
}

// runLedgerTotals sums each run's whole billed footprint per tenant, window or
// not, so it can be compared against the one debit that run wrote.
func runLedgerTotals(ctx context.Context, q pgQuerier, runIDs []string) (map[runKey]runLedgerTotal, error) {
	totals := map[runKey]runLedgerTotal{}
	if len(runIDs) == 0 {
		return totals, nil
	}
	rows, err := q.Query(ctx, `
		SELECT tenant_id, billing_run_id, count(*), COALESCE(sum(billed_amount),0)::text
		FROM usage_ledger
		WHERE billing_state='billed' AND billing_run_id = ANY($1)
		GROUP BY tenant_id, billing_run_id`, runIDs)
	if err != nil {
		return nil, fmt.Errorf("sum ledger rows by run: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key runKey
		var total runLedgerTotal
		var amount string
		if err := rows.Scan(&key.tenantID, &key.runID, &total.rows, &amount); err != nil {
			return nil, err
		}
		total.amount = contracts.Decimal(amount)
		totals[key] = total
	}
	return totals, rows.Err()
}

// runWalletDebits returns each run's debit as a positive amount, so it lines
// up with the ledger sum without either side flipping a sign mid-comparison.
func runWalletDebits(ctx context.Context, q pgQuerier, runIDs []string) (map[runKey]contracts.Decimal, error) {
	debits := map[runKey]contracts.Decimal{}
	if len(runIDs) == 0 {
		return debits, nil
	}
	rows, err := q.Query(ctx, `
		SELECT tenant_id, billing_run_id, COALESCE(sum(-amount),0)::text
		FROM wallet_transactions
		WHERE kind='debit' AND billing_run_id = ANY($1)
		GROUP BY tenant_id, billing_run_id`, runIDs)
	if err != nil {
		return nil, fmt.Errorf("sum wallet debits by run: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key runKey
		var amount string
		if err := rows.Scan(&key.tenantID, &key.runID, &amount); err != nil {
			return nil, err
		}
		debits[key] = contracts.Decimal(amount)
	}
	return debits, rows.Err()
}

type walletTotal struct {
	balance    contracts.Decimal
	journalSum contracts.Decimal
}

func walletTotals(ctx context.Context, q pgQuerier, tenantIDs []string) (map[string]walletTotal, error) {
	totals := map[string]walletTotal{}
	if len(tenantIDs) == 0 {
		return totals, nil
	}
	rows, err := q.Query(ctx, `
		SELECT w.tenant_id, w.balance::text, COALESCE(sum(t.amount),0)::text
		FROM tenant_wallets w
		LEFT JOIN wallet_transactions t ON t.tenant_id = w.tenant_id
		WHERE w.tenant_id = ANY($1)
		GROUP BY w.tenant_id, w.balance`, tenantIDs)
	if err != nil {
		return nil, fmt.Errorf("read wallet totals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var tenantID, balance, journal string
		if err := rows.Scan(&tenantID, &balance, &journal); err != nil {
			return nil, err
		}
		totals[tenantID] = walletTotal{balance: contracts.Decimal(balance), journalSum: contracts.Decimal(journal)}
	}
	return totals, rows.Err()
}

// sameMoney compares two decimals by value, so "1.5" and "1.500000000000" are
// the same amount. Comparing the strings would report a difference on every
// number PostgreSQL and Go happen to spell differently.
func sameMoney(left, right contracts.Decimal) bool {
	a, aok := new(big.Rat).SetString(string(left))
	b, bok := new(big.Rat).SetString(string(right))
	return aok && bok && a.Cmp(b) == 0
}

func zeroMoney() contracts.Decimal {
	return contracts.Decimal(new(big.Rat).FloatString(moneyScale))
}
