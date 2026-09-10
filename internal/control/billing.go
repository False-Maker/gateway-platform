package control

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// moneyScale is the fractional precision of the NUMERIC(38,12) money columns
// in migrations/005_billing.sql. A value with more digits would be silently
// rounded by PostgreSQL, so it is rejected instead.
const moneyScale = 12

// ErrPriceNotEffectiveInFuture is returned when a price would take effect at
// or before the moment it is inserted. D3: a price change only applies
// forward, so a row that is already in effect on arrival could retroactively
// change usage that has already been billed.
var ErrPriceNotEffectiveInFuture = errors.New("model price effective_from must be later than the insert time")

// ErrWalletNotFound is returned when a wallet movement targets a tenant that
// has no wallet row. Wallets are created explicitly, never on the fly during a
// debit, so a missing wallet is a real error rather than a zero balance.
var ErrWalletNotFound = errors.New("tenant wallet does not exist")

// ModelPrice is one immutable price row. Once inserted it is never updated or
// deleted: a change is a new row with a later EffectiveFrom, which is what
// makes a past bill replayable against the price that was actually in effect.
type ModelPrice struct {
	ID              string
	Provider        string
	Model           string
	Currency        string
	UnitScale       int64
	PriceInput      contracts.Decimal
	PriceOutput     contracts.Decimal
	PriceCacheRead  contracts.Decimal
	PriceCacheWrite contracts.Decimal
	EffectiveFrom   time.Time
	CreatedAt       time.Time
}

// WalletMovement is one entry to append to a tenant's wallet journal.
// BalanceAfter is not an input: it is computed by the database inside the same
// transaction that moves the balance, so the journal can never disagree with
// tenant_wallets.balance.
type WalletMovement struct {
	ID           string
	TenantID     string
	Kind         string
	Amount       contracts.Decimal
	BillingRunID string
	Note         string
}

// WalletTransaction is a persisted journal entry.
type WalletTransaction struct {
	ID           string
	TenantID     string
	Kind         string
	Amount       contracts.Decimal
	BalanceAfter contracts.Decimal
	BillingRunID string
	Note         string
	CreatedAt    time.Time
}

// PGBillingRepository owns the B4 money tables. It deliberately exposes no way
// to update or delete a price row, and no way to write tenant_wallets.balance
// without also appending to wallet_transactions.
//
// All amounts cross the wire as contracts.Decimal and are cast to/from
// NUMERIC in SQL (`$n::numeric`, `col::text`); no money value is ever parsed
// into a float64.
type PGBillingRepository struct{ DB *pgxpool.Pool }

// InsertModelPrice appends a price that takes effect in the future. It fails
// if the price is already in effect at `now`, if any amount is not an exact
// decimal representable by the column, or if the same (provider, model,
// effective_from) already exists.
func (r PGBillingRepository) InsertModelPrice(ctx context.Context, price ModelPrice, now time.Time) error {
	if r.DB == nil {
		return errors.New("billing database pool is nil")
	}
	if price.ID == "" || price.Provider == "" || price.Model == "" {
		return errors.New("model price needs an id, provider and model")
	}
	if price.Currency == "" {
		price.Currency = "USD"
	}
	if price.UnitScale <= 0 {
		return fmt.Errorf("model price unit_scale must be positive, got %d", price.UnitScale)
	}
	if !price.EffectiveFrom.After(now) {
		return fmt.Errorf("%w: %s is not after %s", ErrPriceNotEffectiveInFuture,
			price.EffectiveFrom.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano))
	}
	amounts := map[string]contracts.Decimal{
		"price_input":       price.PriceInput,
		"price_output":      price.PriceOutput,
		"price_cache_read":  price.PriceCacheRead,
		"price_cache_write": price.PriceCacheWrite,
	}
	for column, amount := range amounts {
		if err := checkMoney(column, amount); err != nil {
			return err
		}
		if sign, ok := amount.Sign(); !ok || sign < 0 {
			return fmt.Errorf("model price %s must not be negative, got %q", column, amount)
		}
	}
	_, err := r.DB.Exec(ctx, `
		INSERT INTO model_prices (id,provider,model,currency,unit_scale,
			price_input,price_output,price_cache_read,price_cache_write,effective_from)
		VALUES ($1,$2,$3,$4,$5,$6::numeric,$7::numeric,$8::numeric,$9::numeric,$10)`,
		price.ID, price.Provider, price.Model, price.Currency, price.UnitScale,
		string(price.PriceInput), string(price.PriceOutput),
		string(price.PriceCacheRead), string(price.PriceCacheWrite), price.EffectiveFrom.UTC())
	if err != nil {
		return fmt.Errorf("insert model price %s: %w", price.ID, err)
	}
	return nil
}

// PriceAt returns the price in effect for a model at a given instant, which
// for B4.2 is the ledger row's occurred_at -- never the time the billing job
// happens to run. The second result is false when no price was in effect yet;
// callers must treat that as `unpriced`, not as zero.
func (r PGBillingRepository) PriceAt(ctx context.Context, provider, model string, at time.Time) (ModelPrice, bool, error) {
	if r.DB == nil {
		return ModelPrice{}, false, errors.New("billing database pool is nil")
	}
	var price ModelPrice
	var input, output, cacheRead, cacheWrite string
	err := r.DB.QueryRow(ctx, `
		SELECT id,provider,model,currency,unit_scale,
			price_input::text,price_output::text,price_cache_read::text,price_cache_write::text,
			effective_from,created_at
		FROM model_prices
		WHERE provider=$1 AND model=$2 AND effective_from <= $3
		ORDER BY effective_from DESC
		LIMIT 1`, provider, model, at.UTC()).
		Scan(&price.ID, &price.Provider, &price.Model, &price.Currency, &price.UnitScale,
			&input, &output, &cacheRead, &cacheWrite, &price.EffectiveFrom, &price.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ModelPrice{}, false, nil
	}
	if err != nil {
		return ModelPrice{}, false, fmt.Errorf("read price for %s/%s: %w", provider, model, err)
	}
	price.PriceInput = contracts.Decimal(input)
	price.PriceOutput = contracts.Decimal(output)
	price.PriceCacheRead = contracts.Decimal(cacheRead)
	price.PriceCacheWrite = contracts.Decimal(cacheWrite)
	return price, true, nil
}

// EnsureWallet creates a tenant's wallet with a zero balance if it does not
// exist. It never resets an existing balance.
func (r PGBillingRepository) EnsureWallet(ctx context.Context, tenantID, currency string) error {
	if r.DB == nil {
		return errors.New("billing database pool is nil")
	}
	if tenantID == "" {
		return errors.New("wallet needs a tenant id")
	}
	if currency == "" {
		currency = "USD"
	}
	if _, err := r.DB.Exec(ctx, `
		INSERT INTO tenant_wallets (tenant_id,currency) VALUES ($1,$2)
		ON CONFLICT (tenant_id) DO NOTHING`, tenantID, currency); err != nil {
		return fmt.Errorf("ensure wallet for %s: %w", tenantID, err)
	}
	return nil
}

// Balance reads a tenant's current balance. The second result is false when
// the tenant has no wallet; that is not the same as a zero balance and callers
// must not conflate them.
func (r PGBillingRepository) Balance(ctx context.Context, tenantID string) (contracts.Decimal, bool, error) {
	if r.DB == nil {
		return "", false, errors.New("billing database pool is nil")
	}
	var balance string
	err := r.DB.QueryRow(ctx, `SELECT balance::text FROM tenant_wallets WHERE tenant_id=$1`, tenantID).Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read wallet balance for %s: %w", tenantID, err)
	}
	return contracts.Decimal(balance), true, nil
}

// ApplyMovement moves the balance and appends the journal entry in one
// transaction. The new balance is computed by PostgreSQL from the stored
// NUMERIC, so balance_after in the journal is exactly the balance any later
// reader sees, and B4.5 can reconcile the two by construction.
func (r PGBillingRepository) ApplyMovement(ctx context.Context, movement WalletMovement) (WalletTransaction, error) {
	if r.DB == nil {
		return WalletTransaction{}, errors.New("billing database pool is nil")
	}
	if movement.ID == "" || movement.TenantID == "" {
		return WalletTransaction{}, errors.New("wallet movement needs an id and tenant id")
	}
	if err := checkMoney("amount", movement.Amount); err != nil {
		return WalletTransaction{}, err
	}
	if err := checkMovementKind(movement.Kind, movement.Amount); err != nil {
		return WalletTransaction{}, err
	}
	tx, err := r.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return WalletTransaction{}, fmt.Errorf("begin wallet movement: %w", err)
	}
	defer tx.Rollback(ctx)

	// Lock the wallet first so concurrent debits serialize: without it two
	// movements could both read the old balance and write conflicting
	// balance_after values into the journal.
	var locked string
	err = tx.QueryRow(ctx, `SELECT balance::text FROM tenant_wallets WHERE tenant_id=$1 FOR UPDATE`, movement.TenantID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return WalletTransaction{}, fmt.Errorf("%w: %s", ErrWalletNotFound, movement.TenantID)
	}
	if err != nil {
		return WalletTransaction{}, fmt.Errorf("lock wallet %s: %w", movement.TenantID, err)
	}

	var balanceAfter string
	if err := tx.QueryRow(ctx, `
		UPDATE tenant_wallets SET balance = balance + $2::numeric, updated_at = now()
		WHERE tenant_id=$1 RETURNING balance::text`,
		movement.TenantID, string(movement.Amount)).Scan(&balanceAfter); err != nil {
		return WalletTransaction{}, fmt.Errorf("move wallet balance for %s: %w", movement.TenantID, err)
	}

	var billingRunID any
	if movement.BillingRunID != "" {
		billingRunID = movement.BillingRunID
	}
	result := WalletTransaction{
		ID:           movement.ID,
		TenantID:     movement.TenantID,
		Kind:         movement.Kind,
		Amount:       movement.Amount,
		BalanceAfter: contracts.Decimal(balanceAfter),
		BillingRunID: movement.BillingRunID,
		Note:         movement.Note,
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO wallet_transactions (id,tenant_id,kind,amount,balance_after,billing_run_id,note)
		VALUES ($1,$2,$3,$4::numeric,$5::numeric,$6,$7)
		RETURNING created_at`,
		movement.ID, movement.TenantID, movement.Kind, string(movement.Amount),
		balanceAfter, billingRunID, movement.Note).Scan(&result.CreatedAt); err != nil {
		return WalletTransaction{}, fmt.Errorf("record wallet transaction %s: %w", movement.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return WalletTransaction{}, fmt.Errorf("commit wallet movement %s: %w", movement.ID, err)
	}
	return result, nil
}

// ListWalletTransactions returns a tenant's journal oldest first.
func (r PGBillingRepository) ListWalletTransactions(ctx context.Context, tenantID string) ([]WalletTransaction, error) {
	if r.DB == nil {
		return nil, errors.New("billing database pool is nil")
	}
	rows, err := r.DB.Query(ctx, `
		SELECT id,tenant_id,kind,amount::text,balance_after::text,COALESCE(billing_run_id,''),note,created_at
		FROM wallet_transactions WHERE tenant_id=$1 ORDER BY created_at, id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list wallet transactions for %s: %w", tenantID, err)
	}
	defer rows.Close()
	var transactions []WalletTransaction
	for rows.Next() {
		var transaction WalletTransaction
		var amount, balanceAfter string
		if err := rows.Scan(&transaction.ID, &transaction.TenantID, &transaction.Kind, &amount, &balanceAfter,
			&transaction.BillingRunID, &transaction.Note, &transaction.CreatedAt); err != nil {
			return nil, err
		}
		transaction.Amount = contracts.Decimal(amount)
		transaction.BalanceAfter = contracts.Decimal(balanceAfter)
		transactions = append(transactions, transaction)
	}
	return transactions, rows.Err()
}

func checkMovementKind(kind string, amount contracts.Decimal) error {
	sign, ok := amount.Sign()
	if !ok {
		return fmt.Errorf("wallet amount %q is not a decimal", amount)
	}
	switch kind {
	case "topup":
		if sign <= 0 {
			return fmt.Errorf("topup amount must be positive, got %q", amount)
		}
	case "debit":
		if sign >= 0 {
			return fmt.Errorf("debit amount must be negative, got %q", amount)
		}
	case "adjustment":
		if sign == 0 {
			return fmt.Errorf("adjustment amount must not be zero")
		}
	default:
		return fmt.Errorf("unsupported wallet transaction kind %q", kind)
	}
	return nil
}

// checkMoney rejects anything the money columns cannot store exactly, so a
// value is never silently rounded on its way into the ledger.
func checkMoney(column string, amount contracts.Decimal) error {
	if !amount.Valid() {
		return fmt.Errorf("%s is not a decimal: %q", column, amount)
	}
	if places := moneyPlaces(amount); places > moneyScale {
		return fmt.Errorf("%s has %d decimal places, more than the %d stored exactly: %q", column, places, moneyScale, amount)
	}
	return nil
}

// moneyPlaces counts the fractional digits a decimal needs, taking any
// exponent into account ("1.5e-3" needs 4).
func moneyPlaces(amount contracts.Decimal) int {
	raw := strings.TrimSpace(string(amount))
	exponent := 0
	if index := strings.IndexAny(raw, "eE"); index >= 0 {
		exponent, _ = strconv.Atoi(raw[index+1:])
		raw = raw[:index]
	}
	places := 0
	if dot := strings.IndexByte(raw, '.'); dot >= 0 {
		places = len(raw) - dot - 1
	}
	if places -= exponent; places > 0 {
		return places
	}
	return 0
}
