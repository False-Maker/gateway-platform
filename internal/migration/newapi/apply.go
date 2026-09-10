package newapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/elucid/gateway-platform/internal/control/credentials"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Apply writes one complete migration run atomically. The source snapshot has
// already been read in a separate read-only transaction; this function only
// receives the immutable plan and writes the target database.
func Apply(ctx context.Context, db *pgxpool.Pool, cipher *credentials.Cipher, plan Plan, runID string) error {
	if db == nil {
		return errors.New("target database is not configured")
	}
	if cipher == nil {
		return errors.New("credential cipher is not configured")
	}
	if runID == "" {
		runID = newRunID()
	}
	tx, err := db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin target migration: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO migration_runs (id,source_system,source_snapshot_at,status,digest,dry_run) VALUES ($1,$2,$3,'running',$4,false)`, runID, plan.SourceSystem, plan.Snapshot.CapturedAt, plan.Summary.SourceDigest); err != nil {
		return fmt.Errorf("insert migration run: %w", err)
	}
	for _, planned := range plan.Accounts {
		if err := applyAccount(ctx, tx, cipher, plan.SourceSystem, planned); err != nil {
			return err
		}
	}
	for _, planned := range plan.Tenants {
		if err := applyTenant(ctx, tx, plan.SourceSystem, planned); err != nil {
			return err
		}
	}
	for _, planned := range plan.TenantTokens {
		if err := applyTenantToken(ctx, tx, planned); err != nil {
			return err
		}
	}
	for _, record := range plan.Records {
		if err := applyRecord(ctx, tx, runID, plan.SourceSystem, record); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE migration_runs SET status='completed',completed_at=now() WHERE id=$1`, runID); err != nil {
		return fmt.Errorf("complete migration run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit target migration: %w", err)
	}
	return nil
}

func applyAccount(ctx context.Context, tx pgx.Tx, cipher *credentials.Cipher, sourceSystem string, planned PlannedAccount) error {
	accountJSON, err := json.Marshal(planned.Account.Profile)
	if err != nil {
		return err
	}
	limitsJSON, err := json.Marshal(planned.Account.Limits.WithDefaults())
	if err != nil {
		return err
	}
	capabilitiesJSON, err := json.Marshal(planned.Account.Capabilities)
	if err != nil {
		return err
	}
	if len(planned.ModelMapping) == 0 {
		planned.ModelMapping = json.RawMessage(`{}`)
	}
	quotaJSON, err := json.Marshal(planned.Account.Quota)
	if err != nil {
		return err
	}
	var accountID string
	var epoch int64
	err = tx.QueryRow(ctx, `INSERT INTO accounts (id,provider,platform,"group",source_system,source_id,status,fence_epoch,base_url,proxy,model_mapping,capabilities,limits,profile,quota) VALUES ($1,$2,$3,$4,$5,$6,$7,1,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT (source_system,source_id) DO UPDATE SET provider=EXCLUDED.provider,platform=EXCLUDED.platform,"group"=EXCLUDED."group",status=EXCLUDED.status,base_url=EXCLUDED.base_url,proxy=EXCLUDED.proxy,model_mapping=EXCLUDED.model_mapping,capabilities=EXCLUDED.capabilities,limits=EXCLUDED.limits,profile=EXCLUDED.profile,quota=EXCLUDED.quota,fence_epoch=accounts.fence_epoch+1,updated_at=now() RETURNING id,fence_epoch`, planned.Account.ID, planned.Account.Provider, planned.Account.Platform, planned.Account.Group, sourceSystem, planned.SourceID, planned.Account.Status, planned.Account.Profile.BaseURL, planned.Proxy, []byte(planned.ModelMapping), capabilitiesJSON, limitsJSON, accountJSON, quotaJSON).Scan(&accountID, &epoch)
	if err != nil {
		return fmt.Errorf("upsert account %s: %w", planned.SourceID, err)
	}
	if accountID != planned.Account.ID {
		return fmt.Errorf("source %s maps to non-deterministic account id %s", planned.SourceID, accountID)
	}
	plaintext, err := json.Marshal(planned.Bundle)
	if err != nil {
		return err
	}
	encrypted, err := cipher.Encrypt(accountID, plaintext)
	if err != nil {
		return fmt.Errorf("encrypt credential for %s: %w", accountID, err)
	}
	credentialID := accountID + ":credential"
	var expiresAt any
	if !planned.Bundle.ExpiresAt.IsZero() {
		expiresAt = planned.Bundle.ExpiresAt
	}
	if _, err := tx.Exec(ctx, `INSERT INTO credentials (id,account_id,kind,encrypted_secret,expires_at) VALUES ($1,$2,$3,$4,$5) ON CONFLICT (account_id) DO UPDATE SET kind=EXCLUDED.kind,encrypted_secret=EXCLUDED.encrypted_secret,expires_at=EXCLUDED.expires_at,version=credentials.version+1,updated_at=now()`, credentialID, accountID, planned.Account.Credential.Kind, encrypted, expiresAt); err != nil {
		return fmt.Errorf("upsert credential for %s: %w", accountID, err)
	}
	if result, err := tx.Exec(ctx, `UPDATE accounts SET credential_id=$1,updated_at=now() WHERE id=$2 AND fence_epoch=$3`, credentialID, accountID, epoch); err != nil {
		return fmt.Errorf("link credential for %s: %w", accountID, err)
	} else if result.RowsAffected() != 1 {
		return fmt.Errorf("fence lost while linking credential for %s", accountID)
	}
	return nil
}

// applyTenant upserts the tenant and its default principal. Name and status
// follow the source on every run; max_concurrency / rpm (A10) are operator
// settings on the target and are deliberately left untouched by a re-apply.
func applyTenant(ctx context.Context, tx pgx.Tx, sourceSystem string, planned PlannedTenant) error {
	if _, err := tx.Exec(ctx, `INSERT INTO tenants (id,name,status,source_system,source_id)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name,status=EXCLUDED.status,
			source_system=EXCLUDED.source_system,source_id=EXCLUDED.source_id,updated_at=now()`,
		planned.TenantID, planned.Name, planned.Status, sourceSystem, planned.SourceID); err != nil {
		return fmt.Errorf("upsert tenant %s: %w", planned.SourceID, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO principals (id,tenant_id,kind,status)
		VALUES ($1,$2,'user',$3)
		ON CONFLICT (id) DO UPDATE SET status=EXCLUDED.status`,
		planned.PrincipalID, planned.TenantID, planned.Status); err != nil {
		return fmt.Errorf("upsert principal %s: %w", planned.SourceID, err)
	}
	// ON CONFLICT never moves a principal between tenants, so verify ownership
	// explicitly rather than silently attaching tokens to someone else's tenant.
	var owner string
	if err := tx.QueryRow(ctx, `SELECT tenant_id FROM principals WHERE id=$1`, planned.PrincipalID).Scan(&owner); err != nil {
		return fmt.Errorf("verify principal %s: %w", planned.SourceID, err)
	}
	if owner != planned.TenantID {
		return fmt.Errorf("principal %s belongs to tenant %s, not %s", planned.PrincipalID, owner, planned.TenantID)
	}
	return nil
}

// applyTenantToken upserts one tenant_tokens row. revoked_at keeps its first
// value across re-applies so a repeat run does not rewrite history; a token
// that the source re-enabled clears it.
func applyTenantToken(ctx context.Context, tx pgx.Tx, planned PlannedTenantToken) error {
	revoked := planned.Status == "revoked"
	if _, err := tx.Exec(ctx, `INSERT INTO tenant_tokens (id,tenant_id,principal_id,token_hash,"group",status,expires_at,revoked_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,CASE WHEN $8 THEN now() END)
		ON CONFLICT (id) DO UPDATE SET tenant_id=EXCLUDED.tenant_id,principal_id=EXCLUDED.principal_id,
			token_hash=EXCLUDED.token_hash,"group"=EXCLUDED."group",status=EXCLUDED.status,expires_at=EXCLUDED.expires_at,
			revoked_at=CASE WHEN $8 THEN COALESCE(tenant_tokens.revoked_at, now()) END`,
		planned.TokenID, planned.TenantID, planned.PrincipalID, planned.TokenHash, planned.Group, planned.Status, planned.ExpiresAt, revoked); err != nil {
		return fmt.Errorf("upsert tenant token %s: %w", planned.SourceID, err)
	}
	return nil
}

func applyRecord(ctx context.Context, tx pgx.Tx, runID, sourceSystem string, record MigrationRecord) error {
	raw, err := json.Marshal(record.RawSummary)
	if err != nil {
		return err
	}
	conversion, err := json.Marshal(record.Conversion)
	if err != nil {
		return err
	}
	status := record.Status
	if status == "" {
		status = "imported"
	}
	var targetID any
	if record.TargetID != "" {
		targetID = record.TargetID
	}
	var rejection any
	if record.RejectionReason != "" {
		rejection = record.RejectionReason
	}
	_, err = tx.Exec(ctx, `INSERT INTO migration_records (run_id,source_system,source_id,record_kind,source_digest,raw_summary,conversion_result,target_id,rejection_reason,status) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT (source_system,source_id,record_kind) DO UPDATE SET run_id=EXCLUDED.run_id,source_digest=EXCLUDED.source_digest,raw_summary=EXCLUDED.raw_summary,conversion_result=EXCLUDED.conversion_result,target_id=EXCLUDED.target_id,rejection_reason=EXCLUDED.rejection_reason,status=EXCLUDED.status,updated_at=now()`, runID, sourceSystem, record.SourceID, record.RecordKind, record.SourceDigest, raw, conversion, targetID, rejection, status)
	if err != nil {
		return fmt.Errorf("write migration record %s/%s: %w", record.RecordKind, record.SourceID, err)
	}
	return nil
}

func newRunID() string {
	var random [12]byte
	if _, err := rand.Read(random[:]); err == nil {
		return "new-api-" + hex.EncodeToString(random[:])
	}
	return fmt.Sprintf("new-api-%d", time.Now().UnixNano())
}
