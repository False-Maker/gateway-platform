package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// billableUsageSource is the only usage this job is willing to charge for.
// B4.3 owns the full UsageSource policy (what to do with `estimated`,
// `missing`, and Partial=true); until it lands those rows are deliberately
// left `pending` and untouched rather than being billed by default. Not
// billing yet is recoverable; billing usage we do not trust is not.
const billableUsageSource = "upstream"

// billingRunInterval paces the deduction job. Billing is deliberately slow and
// batched: it is never on a request's path, and a longer interval only delays
// a debit, it never loses one.
const billingRunInterval = 5 * time.Minute

// BillingJob is control's slow-path deduction job. Gateway has no part in it:
// gateway writes usage_ledger rows and never reads a price or a balance.
type BillingJob struct {
	DB *pgxpool.Pool
}

// BillingRunResult reports what one invocation settled. Skipped is true when
// the job deliberately did nothing.
type BillingRunResult struct {
	RunID         string
	WindowEnd     time.Time
	Skipped       bool
	SkipReason    string
	TenantsBilled int
	TenantsFailed int
	RowsBilled    int
	RowsUnpriced  int
	TotalDebited  contracts.Decimal
}

type billableRow struct {
	id               int64
	eventID          string
	provider         string
	model            string
	tokensIn         int64
	tokensOut        int64
	cacheReadTokens  int64
	cacheWriteTokens int64
	occurredAt       time.Time
}

// RunOnce settles every eligible ledger row that occurred before windowEnd.
// Each tenant is settled in its own transaction: the wallet debit and the
// "these rows are billed" update either both land or neither does, so a
// crashed run leaves rows pending rather than charged-but-unmarked.
//
// Re-running is safe by construction: a settled row is no longer `pending`,
// so a second run selects nothing for it.
func (j BillingJob) RunOnce(ctx context.Context, windowEnd time.Time) (BillingRunResult, error) {
	if j.DB == nil {
		return BillingRunResult{}, errors.New("billing database pool is nil")
	}
	windowEnd = windowEnd.UTC()
	result := BillingRunResult{RunID: newBillingRunID(), WindowEnd: windowEnd, TotalDebited: "0"}

	// Deployment-order guard: with an empty price table every row would be
	// settled as `unpriced`, which D3 makes permanent (a price can only take
	// effect in the future, so those rows could never be priced afterwards).
	// Shipping the job before the prices are loaded must not destroy revenue.
	var priceCount int
	if err := j.DB.QueryRow(ctx, `SELECT count(*) FROM model_prices`).Scan(&priceCount); err != nil {
		return result, fmt.Errorf("count model prices: %w", err)
	}
	if priceCount == 0 {
		result.Skipped = true
		result.SkipReason = "no model prices are configured"
		if _, err := j.DB.Exec(ctx, `INSERT INTO billing_runs (id,window_end,status,completed_at) VALUES ($1,$2,'skipped',now())`, result.RunID, windowEnd); err != nil {
			return result, fmt.Errorf("record skipped billing run: %w", err)
		}
		return result, nil
	}

	if _, err := j.DB.Exec(ctx, `INSERT INTO billing_runs (id,window_end,status) VALUES ($1,$2,'running')`, result.RunID, windowEnd); err != nil {
		return result, fmt.Errorf("open billing run: %w", err)
	}

	tenants, err := j.pendingTenants(ctx, windowEnd)
	if err != nil {
		j.closeRun(ctx, result, "failed")
		return result, err
	}

	total := new(big.Rat)
	var firstErr error
	for _, tenantID := range tenants {
		settled, err := j.settleTenant(ctx, result.RunID, tenantID, windowEnd)
		if err != nil {
			// One tenant's problem (a missing wallet, say) must not stop the
			// others: its rows stay pending and are retried next run.
			result.TenantsFailed++
			if firstErr == nil {
				firstErr = fmt.Errorf("settle tenant %s: %w", tenantID, err)
			}
			continue
		}
		result.RowsBilled += settled.rowsBilled
		result.RowsUnpriced += settled.rowsUnpriced
		if settled.rowsBilled > 0 || settled.rowsUnpriced > 0 {
			result.TenantsBilled++
		}
		total.Add(total, settled.debited)
	}
	result.TotalDebited = contracts.Decimal(total.FloatString(moneyScale))

	status := "completed"
	if result.TenantsFailed > 0 {
		status = "failed"
	}
	if err := j.closeRun(ctx, result, status); err != nil && firstErr == nil {
		firstErr = err
	}
	return result, firstErr
}

func (j BillingJob) pendingTenants(ctx context.Context, windowEnd time.Time) ([]string, error) {
	rows, err := j.DB.Query(ctx, `
		SELECT DISTINCT tenant_id FROM usage_ledger
		WHERE billing_state='pending' AND occurred_at < $1
		  AND usage_source=$2 AND partial=false
		ORDER BY tenant_id`, windowEnd, billableUsageSource)
	if err != nil {
		return nil, fmt.Errorf("list tenants with pending usage: %w", err)
	}
	defer rows.Close()
	var tenants []string
	for rows.Next() {
		var tenantID string
		if err := rows.Scan(&tenantID); err != nil {
			return nil, err
		}
		tenants = append(tenants, tenantID)
	}
	return tenants, rows.Err()
}

type tenantSettlement struct {
	rowsBilled   int
	rowsUnpriced int
	debited      *big.Rat
}

func (j BillingJob) settleTenant(ctx context.Context, runID, tenantID string, windowEnd time.Time) (tenantSettlement, error) {
	settlement := tenantSettlement{debited: new(big.Rat)}
	tx, err := j.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return settlement, fmt.Errorf("begin settlement: %w", err)
	}
	defer tx.Rollback(ctx)

	rows, err := selectBillableRows(ctx, tx, tenantID, windowEnd)
	if err != nil {
		return settlement, err
	}
	if len(rows) == 0 {
		return settlement, nil
	}

	billed := make(map[int64]contracts.Decimal, len(rows))
	var unpriced []int64
	for _, row := range rows {
		price, found, err := priceAt(ctx, tx, row.provider, row.model, row.occurredAt)
		if err != nil {
			return settlement, err
		}
		if !found {
			unpriced = append(unpriced, row.id)
			continue
		}
		amount, err := lineAmount(row, price)
		if err != nil {
			return settlement, fmt.Errorf("price event %s: %w", row.eventID, err)
		}
		// Round once, per row, and accumulate the rounded value: the wallet
		// debit must equal the sum of the billed_amount column exactly, or
		// B4.5's reconciliation would carry an unexplainable remainder.
		rounded := amount.FloatString(moneyScale)
		exact, ok := new(big.Rat).SetString(rounded)
		if !ok {
			return settlement, fmt.Errorf("event %s produced an unusable amount %q", row.eventID, rounded)
		}
		settlement.debited.Add(settlement.debited, exact)
		billed[row.id] = contracts.Decimal(rounded)
	}

	// A zero total means every eligible row cost nothing (no tokens, or a
	// zero price). Those rows are settled, but no journal entry is written:
	// a zero-amount transaction would be noise in the reconciliation.
	if settlement.debited.Sign() > 0 {
		amount := "-" + settlement.debited.FloatString(moneyScale)
		if _, err := applyMovement(ctx, tx, WalletMovement{
			// Deterministic in (run, tenant): even if a row were somehow
			// selected twice, the primary key would refuse the second debit.
			ID:           runID + ":" + tenantID,
			TenantID:     tenantID,
			Kind:         "debit",
			Amount:       contracts.Decimal(amount),
			BillingRunID: runID,
			Note:         fmt.Sprintf("billing run %s settled %d usage rows", runID, len(billed)),
		}); err != nil {
			return settlement, err
		}
	}

	for id, amount := range billed {
		if _, err := tx.Exec(ctx, `
			UPDATE usage_ledger SET billing_state='billed', billed_amount=$2::numeric, billing_run_id=$3
			WHERE id=$1`, id, string(amount), runID); err != nil {
			return settlement, fmt.Errorf("mark ledger row %d billed: %w", id, err)
		}
	}
	if len(unpriced) > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE usage_ledger SET billing_state='unpriced', billing_run_id=$2
			WHERE id = ANY($1)`, unpriced, runID); err != nil {
			return settlement, fmt.Errorf("mark ledger rows unpriced: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return settlement, fmt.Errorf("commit settlement: %w", err)
	}
	settlement.rowsBilled = len(billed)
	settlement.rowsUnpriced = len(unpriced)
	return settlement, nil
}

func selectBillableRows(ctx context.Context, tx pgx.Tx, tenantID string, windowEnd time.Time) ([]billableRow, error) {
	// FOR UPDATE holds the rows for the whole settlement, so two concurrent
	// runs cannot both price the same usage.
	rows, err := tx.Query(ctx, `
		SELECT id,event_id,provider,model,tokens_in,tokens_out,cache_read_tokens,cache_write_tokens,occurred_at
		FROM usage_ledger
		WHERE tenant_id=$1 AND billing_state='pending' AND occurred_at < $2
		  AND usage_source=$3 AND partial=false
		ORDER BY occurred_at, id
		FOR UPDATE`, tenantID, windowEnd, billableUsageSource)
	if err != nil {
		return nil, fmt.Errorf("select billable rows for %s: %w", tenantID, err)
	}
	defer rows.Close()
	var billable []billableRow
	for rows.Next() {
		var row billableRow
		if err := rows.Scan(&row.id, &row.eventID, &row.provider, &row.model,
			&row.tokensIn, &row.tokensOut, &row.cacheReadTokens, &row.cacheWriteTokens, &row.occurredAt); err != nil {
			return nil, err
		}
		billable = append(billable, row)
	}
	return billable, rows.Err()
}

// lineAmount is the exact cost of one ledger row: the four token counts times
// their respective unit prices, divided by the price's unit_scale. It is a
// pure function of the row and the price that was in effect at the row's
// occurred_at, which is what makes a bill replayable.
//
// The arithmetic is big.Rat, never float64; the result is rounded to
// moneyScale digits once, at the end, when it is written out.
func lineAmount(row billableRow, price ModelPrice) (*big.Rat, error) {
	if price.UnitScale <= 0 {
		return nil, fmt.Errorf("price %s has a non-positive unit_scale %d", price.ID, price.UnitScale)
	}
	total := new(big.Rat)
	for _, part := range []struct {
		tokens int64
		unit   contracts.Decimal
		label  string
	}{
		{row.tokensIn, price.PriceInput, "price_input"},
		{row.tokensOut, price.PriceOutput, "price_output"},
		{row.cacheReadTokens, price.PriceCacheRead, "price_cache_read"},
		{row.cacheWriteTokens, price.PriceCacheWrite, "price_cache_write"},
	} {
		if part.tokens == 0 {
			continue
		}
		if part.tokens < 0 {
			return nil, fmt.Errorf("negative token count %d for %s", part.tokens, part.label)
		}
		unit, ok := new(big.Rat).SetString(string(part.unit))
		if !ok {
			return nil, fmt.Errorf("price %s has an unparseable %s %q", price.ID, part.label, part.unit)
		}
		total.Add(total, unit.Mul(unit, new(big.Rat).SetInt64(part.tokens)))
	}
	return total.Quo(total, new(big.Rat).SetInt64(price.UnitScale)), nil
}

func (j BillingJob) closeRun(ctx context.Context, result BillingRunResult, status string) error {
	if _, err := j.DB.Exec(ctx, `
		UPDATE billing_runs SET status=$2,tenants_billed=$3,tenants_failed=$4,
			rows_billed=$5,rows_unpriced=$6,total_debited=$7::numeric,completed_at=now()
		WHERE id=$1`,
		result.RunID, status, result.TenantsBilled, result.TenantsFailed,
		result.RowsBilled, result.RowsUnpriced, string(result.TotalDebited)); err != nil {
		return fmt.Errorf("close billing run %s: %w", result.RunID, err)
	}
	return nil
}

func newBillingRunID() string {
	var random [12]byte
	if _, err := rand.Read(random[:]); err == nil {
		return "bill-" + hex.EncodeToString(random[:])
	}
	return fmt.Sprintf("bill-%d", time.Now().UnixNano())
}
