package control

import (
	"context"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/elucid/gateway-platform/internal/observability"
	"github.com/elucid/gateway-platform/internal/snapshot"
	"github.com/elucid/gateway-platform/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

func scrape(t *testing.T, registry *observability.Registry) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	return recorder.Body.String()
}

// gaugeValue reads one unlabelled gauge out of a scrape. The gauges under test
// count every tenant in the shared test database, so the assertions below have
// to be about the change this test causes, not about an absolute number that
// another test's fixtures would move.
func gaugeValue(t *testing.T, registry *observability.Registry, name string) float64 {
	t.Helper()
	for _, line := range strings.Split(scrape(t, registry), "\n") {
		if !strings.HasPrefix(line, name+" ") {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimPrefix(line, name+" "), 64)
		if err != nil {
			t.Fatalf("gauge %s = %q: %v", name, line, err)
		}
		return value
	}
	t.Fatalf("gauge %s was never published", name)
	return 0
}

// B4.4: a wallet that goes to zero has to reach the gateway through the token
// snapshot alone, within one publish tick, and it must not sweep up tenants who
// have no wallet at all.
func TestWalletExhaustionReachesTheTokenSnapshotWithinOneTick(t *testing.T) {
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

	const prepaid = "b44-prepaid-tenant"
	const walletless = "b44-walletless-tenant"
	cleanup := func() {
		background := context.Background()
		for _, id := range []string{prepaid, walletless} {
			db.Exec(background, `DELETE FROM wallet_transactions WHERE tenant_id=$1`, id)
			db.Exec(background, `DELETE FROM tenant_wallets WHERE tenant_id=$1`, id)
			db.Exec(background, `DELETE FROM tenants WHERE id=$1`, id)
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	_, prepaidToken, err := CreateTenantToken(ctx, db, prepaid, "B4.4 prepaid", prepaid+"-p", "default", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, walletlessToken, err := CreateTenantToken(ctx, db, walletless, "B4.4 walletless", walletless+"-p", "default", nil)
	if err != nil {
		t.Fatal(err)
	}
	repo := PGBillingRepository{DB: db}
	if err := repo.EnsureWallet(ctx, prepaid, "USD"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ApplyMovement(ctx, WalletMovement{ID: "b44-topup", TenantID: prepaid, Kind: "topup", Amount: "1.5"}); err != nil {
		t.Fatal(err)
	}

	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	registry := observability.NewRegistry()
	loop := TokenLoop{Repository: PGTenantRepository{DB: db}, Redis: rdb, Metrics: registry}

	if err := loop.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	baselineBlocked := gaugeValue(t, registry, "control_billing_blocked_tenants")
	published, err := snapshot.LoadTokens(ctx, rdb)
	if err != nil {
		t.Fatal(err)
	}
	if record := published[snapshot.HashToken(prepaidToken)]; record.BillingBlocked {
		t.Fatal("a funded wallet was published as blocked")
	}
	// Absence of a wallet is not the same as an empty wallet. Blocking here
	// would take down every tenant who is billed some other way.
	if record := published[snapshot.HashToken(walletlessToken)]; record.BillingBlocked {
		t.Fatal("a tenant with no wallet row was published as blocked")
	}

	// Spend the balance down to exactly zero: the boundary is inclusive, a
	// tenant at 0.000000000000 has nothing left to spend.
	if _, err := repo.ApplyMovement(ctx, WalletMovement{ID: "b44-debit", TenantID: prepaid, Kind: "debit", Amount: "-1.5"}); err != nil {
		t.Fatal(err)
	}
	balance, _, err := repo.Balance(ctx, prepaid)
	if err != nil {
		t.Fatal(err)
	}
	if balance != "0.000000000000" {
		t.Fatalf("balance = %q, want exactly zero", balance)
	}

	// One tick. Not two, not "eventually": the DoD bounds the delay at a single
	// snapshot period, and that is what makes the overdraft bounded.
	if err := loop.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	published, err = snapshot.LoadTokens(ctx, rdb)
	if err != nil {
		t.Fatal(err)
	}
	if record := published[snapshot.HashToken(prepaidToken)]; !record.BillingBlocked {
		t.Fatalf("an exhausted wallet was not published as blocked: %#v", record)
	}
	if record := published[snapshot.HashToken(walletlessToken)]; record.BillingBlocked {
		t.Fatal("the walletless tenant was blocked by its neighbour's debt")
	}

	if got := gaugeValue(t, registry, "control_billing_blocked_tenants"); got != baselineBlocked+1 {
		t.Errorf("blocked-tenant gauge = %v, want %v (one more than before)", got, baselineBlocked+1)
	}

	// A top-up releases the block on the next tick, by the same one path.
	if _, err := repo.ApplyMovement(ctx, WalletMovement{ID: "b44-refill", TenantID: prepaid, Kind: "topup", Amount: "2"}); err != nil {
		t.Fatal(err)
	}
	if err := loop.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	published, _ = snapshot.LoadTokens(ctx, rdb)
	if record := published[snapshot.HashToken(prepaidToken)]; record.BillingBlocked {
		t.Fatal("a refilled wallet stayed blocked")
	}
	if got := gaugeValue(t, registry, "control_billing_blocked_tenants"); got != baselineBlocked {
		t.Errorf("blocked-tenant gauge = %v after the refill, want the baseline %v", got, baselineBlocked)
	}
}

// The overdraft this design accepts is bounded by the tenant's rpm. A prepaid
// tenant with rpm = 0 has no bound at all, so control must be able to name them.
func TestUncappedWalletTenantsNamesPrepaidTenantsWithNoRateLimit(t *testing.T) {
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

	const uncapped = "b44-uncapped-tenant"
	const capped = "b44-capped-tenant"
	const noWallet = "b44-nowallet-tenant"
	cleanup := func() {
		background := context.Background()
		for _, id := range []string{uncapped, capped, noWallet} {
			db.Exec(background, `DELETE FROM tenant_wallets WHERE tenant_id=$1`, id)
			db.Exec(background, `DELETE FROM tenants WHERE id=$1`, id)
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	repo := PGBillingRepository{DB: db}
	sixty := 60
	for _, spec := range []struct {
		id     string
		rpm    *int
		wallet bool
	}{
		{uncapped, nil, true},
		{capped, &sixty, true},
		{noWallet, nil, false},
	} {
		if _, _, err := CreateTenantTokenWithLimits(ctx, db, spec.id, "", spec.id+"-p", "default", nil, TenantLimitSpec{RPM: spec.rpm}); err != nil {
			t.Fatal(err)
		}
		if spec.wallet {
			if err := repo.EnsureWallet(ctx, spec.id, "USD"); err != nil {
				t.Fatal(err)
			}
		}
	}

	tenants, err := PGTenantRepository{DB: db}.UncappedWalletTenants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, id := range tenants {
		found[id] = true
	}
	if !found[uncapped] {
		t.Errorf("a prepaid tenant with rpm=0 was not reported: %v", tenants)
	}
	if found[capped] {
		t.Error("a rate-limited tenant was reported as uncapped")
	}
	// No wallet means no prepaid overdraft to be unbounded about.
	if found[noWallet] {
		t.Error("a tenant with no wallet was reported as an overdraft risk")
	}
}
