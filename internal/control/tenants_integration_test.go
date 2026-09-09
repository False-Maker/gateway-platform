package control

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elucid/gateway-platform/internal/snapshot"
	"github.com/elucid/gateway-platform/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

func TestPGTenantTokensPublishAndRevoke(t *testing.T) {
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
	const tenantID = "a6-tenant-fixture"
	if _, err := db.Exec(ctx, `DELETE FROM tenants WHERE id=$1`, tenantID); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(context.Background(), `DELETE FROM tenants WHERE id=$1`, tenantID)

	past := time.Now().Add(-time.Hour)
	liveID, liveToken, err := CreateTenantToken(ctx, db, tenantID, "A6", tenantID+"-p", "vip", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, expiredToken, err := CreateTenantToken(ctx, db, tenantID, "", tenantID+"-p", "vip", &past)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := CreateTenantToken(ctx, db, "other-tenant", "", tenantID+"-p", "vip", nil); err == nil {
		t.Fatal("principal owned by another tenant was accepted")
	}
	var stored string
	if err := db.QueryRow(ctx, `SELECT token_hash FROM tenant_tokens WHERE id=$1`, liveID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == liveToken || stored != snapshot.HashToken(liveToken) {
		t.Fatalf("token stored as %q, want SHA-256 of the raw token", stored)
	}

	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	loop := TokenLoop{Repository: PGTenantRepository{DB: db}, Redis: rdb}
	if err := loop.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	published, err := snapshot.LoadTokens(ctx, rdb)
	if err != nil {
		t.Fatal(err)
	}
	live, ok := published[snapshot.HashToken(liveToken)]
	if !ok || live.TenantID != tenantID || live.Group != "vip" || live.PrincipalID != tenantID+"-p" || live.MaxConcurrency != 0 || live.RPM != 0 {
		t.Fatalf("live token record=%#v ok=%t", live, ok)
	}
	// A10: limits set at creation are published; nil fields keep existing values.
	four, sixty := 4, 60
	if _, _, err := CreateTenantTokenWithLimits(ctx, db, tenantID, "", tenantID+"-p", "vip", nil, TenantLimitSpec{MaxConcurrency: &four, RPM: &sixty}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CreateTenantTokenWithLimits(ctx, db, tenantID, "", tenantID+"-p", "vip", nil, TenantLimitSpec{RPM: &four}); err != nil {
		t.Fatal(err)
	}
	negative := -1
	if _, _, err := CreateTenantTokenWithLimits(ctx, db, tenantID, "", tenantID+"-p", "vip", nil, TenantLimitSpec{RPM: &negative}); err == nil {
		t.Fatal("negative tenant limit accepted")
	}
	if err := loop.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	published, _ = snapshot.LoadTokens(ctx, rdb)
	if live := published[snapshot.HashToken(liveToken)]; live.MaxConcurrency != 4 || live.RPM != 4 {
		t.Fatalf("tenant limits not published (concurrency kept, rpm updated): %#v", live)
	}
	if _, ok := published[snapshot.HashToken(expiredToken)]; ok {
		t.Fatal("expired token was published")
	}

	if _, err := db.Exec(ctx, `UPDATE tenant_tokens SET status='revoked', revoked_at=now() WHERE id=$1`, liveID); err != nil {
		t.Fatal(err)
	}
	if err := loop.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	published, _ = snapshot.LoadTokens(ctx, rdb)
	if _, ok := published[snapshot.HashToken(liveToken)]; ok {
		t.Fatal("revoked token survived republish")
	}

	if _, err := db.Exec(ctx, `UPDATE tenant_tokens SET status='active', revoked_at=NULL WHERE id=$1`, liveID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE tenants SET status='suspended' WHERE id=$1`, tenantID); err != nil {
		t.Fatal(err)
	}
	if err := loop.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	published, _ = snapshot.LoadTokens(ctx, rdb)
	if _, ok := published[snapshot.HashToken(liveToken)]; ok {
		t.Fatal("suspended tenant's token was published")
	}
}
