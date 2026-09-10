package control

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/elucid/gateway-platform/migrations"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

func openBillingTestDB(t *testing.T) (context.Context, *pgxpool.Pool) {
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
	// Applying twice must be a no-op: every file, including 005, has to stay
	// idempotent because migrations.Apply replays all of them on every boot.
	if err := migrations.Apply(ctx, db); err != nil {
		t.Fatalf("re-applying migrations is not idempotent: %v", err)
	}
	return ctx, db
}

// purgeTestPrices removes this test's fixture prices. It has to disable the
// immutability trigger to do so, which is the point: outside of explicit DDL
// by the table owner there is no way to erase a price row.
func purgeTestPrices(ctx context.Context, db *pgxpool.Pool, provider string) error {
	if _, err := db.Exec(ctx, `ALTER TABLE model_prices DISABLE TRIGGER model_prices_immutable_trigger`); err != nil {
		return err
	}
	_, deleteErr := db.Exec(ctx, `DELETE FROM model_prices WHERE provider=$1`, provider)
	if _, err := db.Exec(ctx, `ALTER TABLE model_prices ENABLE TRIGGER model_prices_immutable_trigger`); err != nil {
		return err
	}
	return deleteErr
}

func TestModelPricesOnlyGoForwardAndAreImmutable(t *testing.T) {
	ctx, db := openBillingTestDB(t)
	repo := PGBillingRepository{DB: db}
	const provider, model = "b41-provider", "b41-model"
	if err := purgeTestPrices(ctx, db, provider); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { purgeTestPrices(context.Background(), db, provider) })

	now := time.Now().UTC()
	base := ModelPrice{
		Provider: provider, Model: model, Currency: "USD", UnitScale: 1000000,
		PriceInput: "1.5", PriceOutput: "7.25", PriceCacheRead: "0.15", PriceCacheWrite: "1.875",
	}

	// A price that is already in effect on arrival would retroactively change
	// usage that may already be billed. It must be refused, with a reason.
	past := base
	past.ID = "b41-past"
	past.EffectiveFrom = now.Add(-time.Minute)
	if err := repo.InsertModelPrice(ctx, past, now); !errors.Is(err, ErrPriceNotEffectiveInFuture) {
		t.Fatalf("backdated price error = %v, want ErrPriceNotEffectiveInFuture", err)
	}
	sameInstant := base
	sameInstant.ID = "b41-same"
	sameInstant.EffectiveFrom = now
	if err := repo.InsertModelPrice(ctx, sameInstant, now); !errors.Is(err, ErrPriceNotEffectiveInFuture) {
		t.Fatalf("effective_from == now error = %v, want ErrPriceNotEffectiveInFuture", err)
	}
	// 13 decimal places cannot be stored exactly by NUMERIC(38,12).
	tooPrecise := base
	tooPrecise.ID = "b41-precise"
	tooPrecise.EffectiveFrom = now.Add(time.Hour)
	tooPrecise.PriceInput = "0.0000000000001"
	if err := repo.InsertModelPrice(ctx, tooPrecise, now); err == nil {
		t.Fatal("a price that PG would round was accepted")
	}

	first := base
	first.ID = "b41-first"
	first.EffectiveFrom = now.Add(time.Hour)
	if err := repo.InsertModelPrice(ctx, first, now); err != nil {
		t.Fatal(err)
	}
	second := base
	second.ID = "b41-second"
	second.EffectiveFrom = now.Add(2 * time.Hour)
	second.PriceInput = "2.5"
	if err := repo.InsertModelPrice(ctx, second, now); err != nil {
		t.Fatal(err)
	}
	// Same (provider, model, effective_from) twice is a conflict, not a
	// silent overwrite of the earlier price.
	duplicate := first
	duplicate.ID = "b41-duplicate"
	duplicate.PriceInput = "99"
	if err := repo.InsertModelPrice(ctx, duplicate, now); err == nil {
		t.Fatal("duplicate effective_from was accepted")
	}

	// Price lookup follows the usage instant, not the job's wall clock.
	if _, found, err := repo.PriceAt(ctx, provider, model, now); err != nil || found {
		t.Fatalf("price before the first effective_from: found=%v err=%v (must be unpriced, not zero)", found, err)
	}
	priced, found, err := repo.PriceAt(ctx, provider, model, now.Add(90*time.Minute))
	if err != nil || !found {
		t.Fatalf("mid-window lookup: found=%v err=%v", found, err)
	}
	if priced.ID != first.ID || priced.PriceInput != contracts.Decimal("1.500000000000") {
		t.Errorf("mid-window price = %s / %q", priced.ID, priced.PriceInput)
	}
	if priced.PriceCacheWrite != contracts.Decimal("1.875000000000") || priced.UnitScale != 1000000 || priced.Currency != "USD" {
		t.Errorf("price round-trip lost precision or metadata: %+v", priced)
	}
	later, found, err := repo.PriceAt(ctx, provider, model, now.Add(3*time.Hour))
	if err != nil || !found || later.ID != second.ID {
		t.Fatalf("later lookup = %s found=%v err=%v", later.ID, found, err)
	}

	// The database refuses to rewrite history even outside the repository.
	if _, err := db.Exec(ctx, `UPDATE model_prices SET price_input=0 WHERE id=$1`, first.ID); err == nil {
		t.Error("model_prices row was updatable")
	}
	if _, err := db.Exec(ctx, `DELETE FROM model_prices WHERE id=$1`, first.ID); err == nil {
		t.Error("model_prices row was deletable")
	}
	var remaining int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM model_prices WHERE provider=$1`, provider).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 2 {
		t.Errorf("price rows = %d, want 2", remaining)
	}
}

func TestWalletBalanceAndJournalMoveInOneTransaction(t *testing.T) {
	ctx, db := openBillingTestDB(t)
	repo := PGBillingRepository{DB: db}
	const tenantID = "b41-wallet-tenant"
	if _, err := db.Exec(ctx, `DELETE FROM tenants WHERE id=$1`, tenantID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(context.Background(), `DELETE FROM tenants WHERE id=$1`, tenantID) })
	if _, err := db.Exec(ctx, `INSERT INTO tenants (id,name) VALUES ($1,'B4.1 wallet fixture')`, tenantID); err != nil {
		t.Fatal(err)
	}

	// A debit against a tenant with no wallet is an error, not an implicit
	// zero balance.
	if _, err := repo.ApplyMovement(ctx, WalletMovement{ID: "b41-nowallet", TenantID: tenantID, Kind: "debit", Amount: "-1"}); !errors.Is(err, ErrWalletNotFound) {
		t.Fatalf("movement without a wallet = %v, want ErrWalletNotFound", err)
	}
	if _, found, err := repo.Balance(ctx, tenantID); err != nil || found {
		t.Fatalf("balance before EnsureWallet: found=%v err=%v", found, err)
	}

	if err := repo.EnsureWallet(ctx, tenantID, "USD"); err != nil {
		t.Fatal(err)
	}
	balance, found, err := repo.Balance(ctx, tenantID)
	if err != nil || !found || balance != contracts.Decimal("0.000000000000") {
		t.Fatalf("fresh wallet balance = %q found=%v err=%v", balance, found, err)
	}

	topup, err := repo.ApplyMovement(ctx, WalletMovement{ID: "b41-topup", TenantID: tenantID, Kind: "topup", Amount: "10.05", Note: "fixture topup"})
	if err != nil {
		t.Fatal(err)
	}
	if topup.BalanceAfter != contracts.Decimal("10.050000000000") {
		t.Errorf("balance after topup = %q", topup.BalanceAfter)
	}
	// 0.01 has no exact float64 representation; adding it a hundred times is
	// the case that would drift if any of this went through a float.
	for index := 0; index < 100; index++ {
		if _, err := repo.ApplyMovement(ctx, WalletMovement{
			ID: "b41-debit-" + strconv.Itoa(index), TenantID: tenantID,
			Kind: "debit", Amount: "-0.01", BillingRunID: "b41-run",
		}); err != nil {
			t.Fatal(err)
		}
	}
	balance, _, err = repo.Balance(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if balance != contracts.Decimal("9.050000000000") {
		t.Fatalf("balance after 100 debits of 0.01 = %q, want exactly 9.050000000000", balance)
	}

	// EnsureWallet must never reset an existing balance.
	if err := repo.EnsureWallet(ctx, tenantID, "USD"); err != nil {
		t.Fatal(err)
	}
	if again, _, err := repo.Balance(ctx, tenantID); err != nil || again != balance {
		t.Fatalf("EnsureWallet changed the balance to %q (err=%v)", again, err)
	}

	// The journal reconciles with the balance by construction: the last
	// balance_after is the balance, and the entries sum to it.
	transactions, err := repo.ListWalletTransactions(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 101 {
		t.Fatalf("journal has %d entries, want 101", len(transactions))
	}
	if transactions[len(transactions)-1].BalanceAfter != balance {
		t.Errorf("last balance_after %q != balance %q", transactions[len(transactions)-1].BalanceAfter, balance)
	}
	if transactions[0].Kind != "topup" || transactions[0].Note != "fixture topup" || transactions[0].BillingRunID != "" {
		t.Errorf("first journal entry = %+v", transactions[0])
	}
	if transactions[1].BillingRunID != "b41-run" {
		t.Errorf("debit lost its billing run id: %+v", transactions[1])
	}
	var journalSum string
	if err := db.QueryRow(ctx, `SELECT COALESCE(sum(amount),0)::text FROM wallet_transactions WHERE tenant_id=$1`, tenantID).Scan(&journalSum); err != nil {
		t.Fatal(err)
	}
	if contracts.Decimal(journalSum) != balance {
		t.Errorf("journal sum %q != balance %q", journalSum, balance)
	}

	// A movement that the guards reject must leave no trace at all.
	before := balance
	if _, err := repo.ApplyMovement(ctx, WalletMovement{ID: "b41-bad-sign", TenantID: tenantID, Kind: "topup", Amount: "-1"}); err == nil {
		t.Error("a negative topup was accepted")
	}
	if _, err := repo.ApplyMovement(ctx, WalletMovement{ID: "b41-bad-kind", TenantID: tenantID, Kind: "refund", Amount: "1"}); err == nil {
		t.Error("an unknown transaction kind was accepted")
	}
	if _, err := repo.ApplyMovement(ctx, WalletMovement{ID: "b41-topup", TenantID: tenantID, Kind: "topup", Amount: "1"}); err == nil {
		t.Error("a duplicate transaction id was accepted")
	}
	after, _, err := repo.Balance(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("rejected movements changed the balance from %q to %q", before, after)
	}
	var journalCount int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM wallet_transactions WHERE tenant_id=$1`, tenantID).Scan(&journalCount); err != nil {
		t.Fatal(err)
	}
	if journalCount != 101 {
		t.Errorf("journal grew to %d entries after rejected movements", journalCount)
	}

	// Deleting the tenant cascades the wallet and its journal away.
	if _, err := db.Exec(ctx, `DELETE FROM tenants WHERE id=$1`, tenantID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM wallet_transactions WHERE tenant_id=$1`, tenantID).Scan(&journalCount); err != nil {
		t.Fatal(err)
	}
	if journalCount != 0 {
		t.Errorf("journal survived the tenant delete with %d entries", journalCount)
	}
}
