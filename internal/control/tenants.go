package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/elucid/gateway-platform/internal/observability"
	"github.com/elucid/gateway-platform/internal/snapshot"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// TenantRepository is control's authoritative view of tenant API tokens.
// Only hashes are stored; the raw token exists in memory once, at creation.
type TenantRepository interface {
	ListActiveTokens(context.Context, time.Time) (map[string]snapshot.TokenRecord, error)
}

type PGTenantRepository struct{ DB *pgxpool.Pool }

func (r PGTenantRepository) ListActiveTokens(ctx context.Context, now time.Time) (map[string]snapshot.TokenRecord, error) {
	if r.DB == nil {
		return nil, errors.New("tenant database pool is nil")
	}
	// B4.4: the wallet is read here, on the 15s publish tick, and nowhere near
	// a request. A tenant with no wallet row is not a prepaid tenant and is
	// never blocked -- absence of a wallet must not read as "no money".
	rows, err := r.DB.Query(ctx, `
		SELECT k.id, k.tenant_id, k.principal_id, k."group", k.token_hash, k.expires_at, t.max_concurrency, t.rpm,
		       (w.tenant_id IS NOT NULL AND w.balance <= 0) AS billing_blocked
		FROM tenant_tokens k
		JOIN tenants t ON t.id = k.tenant_id
		JOIN principals p ON p.id = k.principal_id
		LEFT JOIN tenant_wallets w ON w.tenant_id = k.tenant_id
		WHERE k.status = 'active' AND k.revoked_at IS NULL
		  AND t.status = 'active' AND p.status = 'active'
		  AND (k.expires_at IS NULL OR k.expires_at > $1)`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make(map[string]snapshot.TokenRecord)
	for rows.Next() {
		var record snapshot.TokenRecord
		var hash string
		var expiresAt *time.Time
		if err := rows.Scan(&record.TokenID, &record.TenantID, &record.PrincipalID, &record.Group, &hash, &expiresAt,
			&record.MaxConcurrency, &record.RPM, &record.BillingBlocked); err != nil {
			return nil, err
		}
		if expiresAt != nil {
			record.ExpiresAt = expiresAt.UTC()
		}
		records[hash] = record
	}
	return records, rows.Err()
}

// CreateTenantToken inserts a tenant, its default principal, and one token in
// a single transaction. It returns the raw token exactly once; only the
// SHA-256 hash is persisted. Re-running with the same tenant/principal IDs is
// idempotent for the tenant and principal rows but always mints a new token.
func CreateTenantToken(ctx context.Context, db *pgxpool.Pool, tenantID, tenantName, principalID, group string, expiresAt *time.Time) (string, string, error) {
	return CreateTenantTokenWithLimits(ctx, db, tenantID, tenantName, principalID, group, expiresAt, TenantLimitSpec{})
}

// TenantLimitSpec sets tenant-wide ceilings at creation. Nil fields leave an
// existing tenant's limits untouched; on a new tenant they default to 0.
type TenantLimitSpec struct {
	MaxConcurrency *int
	RPM            *int
}

func CreateTenantTokenWithLimits(ctx context.Context, db *pgxpool.Pool, tenantID, tenantName, principalID, group string, expiresAt *time.Time, limits TenantLimitSpec) (string, string, error) {
	if db == nil {
		return "", "", errors.New("database pool is nil")
	}
	tenantID, principalID, group = strings.TrimSpace(tenantID), strings.TrimSpace(principalID), strings.TrimSpace(group)
	if tenantID == "" || principalID == "" {
		return "", "", errors.New("tenant_id and principal_id are required")
	}
	if group == "" {
		group = "default"
	}
	if len(tenantID) > 128 || len(principalID) > 128 || len(group) > 128 {
		return "", "", errors.New("tenant identifiers are too large")
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", "", err
	}
	rawToken := "gwt_" + hex.EncodeToString(secret[:])
	hash := snapshot.HashToken(rawToken)
	var idBytes [8]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return "", "", err
	}
	tokenID := "token-" + hex.EncodeToString(idBytes[:])
	tx, err := db.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)
	if tenantName == "" {
		tenantName = tenantID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tenants (id,name) VALUES ($1,$2) ON CONFLICT (id) DO NOTHING`, tenantID, tenantName); err != nil {
		return "", "", fmt.Errorf("insert tenant: %w", err)
	}
	if limits.MaxConcurrency != nil || limits.RPM != nil {
		if (limits.MaxConcurrency != nil && *limits.MaxConcurrency < 0) || (limits.RPM != nil && *limits.RPM < 0) {
			return "", "", errors.New("tenant limits must be non-negative")
		}
		if _, err := tx.Exec(ctx, `UPDATE tenants SET max_concurrency=COALESCE($2,max_concurrency), rpm=COALESCE($3,rpm), updated_at=now() WHERE id=$1`, tenantID, limits.MaxConcurrency, limits.RPM); err != nil {
			return "", "", fmt.Errorf("update tenant limits: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO principals (id,tenant_id) VALUES ($1,$2) ON CONFLICT (id) DO NOTHING`, principalID, tenantID); err != nil {
		return "", "", fmt.Errorf("insert principal: %w", err)
	}
	var owner string
	if err := tx.QueryRow(ctx, `SELECT tenant_id FROM principals WHERE id=$1`, principalID).Scan(&owner); err != nil {
		return "", "", fmt.Errorf("verify principal: %w", err)
	}
	if owner != tenantID {
		return "", "", fmt.Errorf("principal %s belongs to tenant %s, not %s", principalID, owner, tenantID)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tenant_tokens (id,tenant_id,principal_id,token_hash,"group",expires_at) VALUES ($1,$2,$3,$4,$5,$6)`, tokenID, tenantID, principalID, hash, group, expiresAt); err != nil {
		return "", "", fmt.Errorf("insert token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", err
	}
	return tokenID, rawToken, nil
}

const tokenRefreshInterval = 15 * time.Second

// TokenLoop publishes the active token set to Redis so gateway can
// authenticate without touching PostgreSQL.
type TokenLoop struct {
	Repository TenantRepository
	Redis      redis.UniversalClient
	Now        func() time.Time
	// Metrics carries the B4.4 overdraft-exposure signals. Optional: a nil
	// registry drops the signals, it does not stop the publish.
	Metrics *observability.Registry
}

// uncappedWalletReporter is implemented by repositories that can also name the
// prepaid tenants whose spend rate has no ceiling. It is a separate optional
// interface so the existing TenantRepository fakes keep compiling; a repository
// that does not implement it simply publishes no exposure signal.
type uncappedWalletReporter interface {
	UncappedWalletTenants(context.Context) ([]string, error)
}

// UncappedWalletTenants lists prepaid tenants with rpm = 0 (unlimited).
//
// B4.0 D5 bounds the overdraft this design accepts at roughly
//
//	peak spend rate x (snapshot period + billing period + in-flight duration)
//
// and the only thing capping the first factor is A10's per-tenant rpm. A
// prepaid tenant with rpm = 0 therefore has an *unbounded* overdraft, which is
// the one way this design fails badly rather than gracefully. It is reported,
// not silently corrected: forcing a limit here would be control inventing a
// number nobody chose.
func (r PGTenantRepository) UncappedWalletTenants(ctx context.Context) ([]string, error) {
	if r.DB == nil {
		return nil, errors.New("tenant database pool is nil")
	}
	rows, err := r.DB.Query(ctx, `
		SELECT t.id FROM tenants t
		JOIN tenant_wallets w ON w.tenant_id = t.id
		WHERE t.status = 'active' AND t.rpm = 0
		ORDER BY t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tenants []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		tenants = append(tenants, id)
	}
	return tenants, rows.Err()
}

func (l TokenLoop) RunOnce(ctx context.Context) error {
	if l.Repository == nil {
		return errors.New("tenant repository is not configured")
	}
	if l.Redis == nil {
		return errors.New("token redis client is nil")
	}
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	records, err := l.Repository.ListActiveTokens(ctx, now().UTC())
	if err != nil {
		return fmt.Errorf("list active tokens: %w", err)
	}
	if err := snapshot.PublishTokens(ctx, l.Redis, records); err != nil {
		return fmt.Errorf("publish tokens: %w", err)
	}
	l.publishExposure(ctx, records)
	return nil
}

// publishExposure reports how many tenants are currently refused, and how many
// prepaid tenants can overdraw without bound. Both are gauges rather than
// counters: what matters is the standing state, not how often it was recomputed.
func (l TokenLoop) publishExposure(ctx context.Context, records map[string]snapshot.TokenRecord) {
	if l.Metrics == nil {
		return
	}
	blocked := map[string]struct{}{}
	for _, record := range records {
		if record.BillingBlocked {
			blocked[record.TenantID] = struct{}{}
		}
	}
	// Tenants, not tokens: one tenant with six tokens is one blocked tenant.
	l.Metrics.SetGauge("control_billing_blocked_tenants", float64(len(blocked)))
	reporter, ok := l.Repository.(uncappedWalletReporter)
	if !ok {
		return
	}
	uncapped, err := reporter.UncappedWalletTenants(ctx)
	if err != nil {
		log.Printf("control uncapped wallet tenant check failed: %v", err)
		return
	}
	l.Metrics.SetGauge("control_billing_uncapped_wallet_tenants", float64(len(uncapped)))
	if len(uncapped) > 0 {
		log.Printf("control: %d prepaid tenants have rpm=0, so their overdraft is unbounded: %v", len(uncapped), uncapped)
	}
}

var _ TenantRepository = PGTenantRepository{}
var _ uncappedWalletReporter = PGTenantRepository{}
