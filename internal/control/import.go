package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/elucid/gateway-platform/internal/control/credentials"
	"github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ImportRecord struct {
	Request         contracts.ImportRequest
	Account         contracts.Account
	EncryptedSecret []byte
}

type ImportRepository interface {
	TryLock(context.Context, string) (func() error, bool, error)
	Save(context.Context, ImportRecord) (string, int64, int64, error)
}

type PGImportRepository struct {
	DB *pgxpool.Pool
}

func (r PGImportRepository) TryLock(ctx context.Context, accountID string) (func() error, bool, error) {
	return (Fencer{DB: r.DB}).TryAcquireSingleton(ctx, accountLockKey(accountID))
}

func (r PGImportRepository) Save(ctx context.Context, record ImportRecord) (string, int64, int64, error) {
	profile, err := json.Marshal(record.Account.Profile)
	if err != nil {
		return "", 0, 0, err
	}
	limits, err := json.Marshal(record.Account.Limits.WithDefaults())
	if err != nil {
		return "", 0, 0, err
	}
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return "", 0, 0, err
	}
	defer tx.Rollback(ctx)
	var accountID string
	var epoch int64
	err = tx.QueryRow(ctx, `INSERT INTO accounts (id,provider,platform,"group",source_system,source_id,status,base_url,proxy,profile,limits) VALUES ($1,$2,$3,$4,$5,$6,'active',$7,$8,$9,$10) ON CONFLICT (source_system,source_id) DO UPDATE SET provider=EXCLUDED.provider,platform=EXCLUDED.platform,"group"=EXCLUDED."group",status='active',base_url=EXCLUDED.base_url,proxy=EXCLUDED.proxy,profile=EXCLUDED.profile,limits=EXCLUDED.limits,fence_epoch=accounts.fence_epoch+1,updated_at=now() RETURNING id,fence_epoch`, record.Account.ID, record.Account.Provider, record.Account.Platform, record.Account.Group, record.Request.SourceSystem, record.Request.SourceID, record.Account.Profile.BaseURL, record.Account.Profile.Proxy, string(profile), string(limits)).Scan(&accountID, &epoch)
	if err != nil {
		return "", 0, 0, err
	}
	if accountID != record.Account.ID {
		return "", 0, 0, errors.New("existing source maps to a non-deterministic account ID")
	}
	credentialID := accountID + ":credential"
	var expiresAt any
	if !record.Account.Credential.ExpiresAt.IsZero() {
		expiresAt = record.Account.Credential.ExpiresAt
	}
	var credentialVersion int64
	err = tx.QueryRow(ctx, `INSERT INTO credentials (id,account_id,kind,encrypted_secret,expires_at) VALUES ($1,$2,$3,$4,$5) ON CONFLICT (account_id) DO UPDATE SET kind=EXCLUDED.kind,encrypted_secret=EXCLUDED.encrypted_secret,expires_at=EXCLUDED.expires_at,version=credentials.version+1,updated_at=now() RETURNING version`, credentialID, accountID, record.Account.Credential.Kind, record.EncryptedSecret, expiresAt).Scan(&credentialVersion)
	if err != nil {
		return "", 0, 0, err
	}
	result, err := tx.Exec(ctx, `UPDATE accounts SET credential_id=$1,updated_at=now() WHERE id=$2 AND fence_epoch=$3`, credentialID, accountID, epoch)
	if err != nil {
		return "", 0, 0, err
	}
	if result.RowsAffected() != 1 {
		return "", 0, 0, ErrFenceLost
	}
	if err := tx.Commit(ctx); err != nil {
		return "", 0, 0, err
	}
	return accountID, epoch, credentialVersion, nil
}

type ImportService struct {
	Repository ImportRepository
	Cipher     *credentials.Cipher
	Provider   func(string) (provider.Provider, bool)
}

func (s ImportService) Import(ctx context.Context, req contracts.ImportRequest) (contracts.Account, error) {
	if s.Cipher == nil {
		return contracts.Account{}, errors.New("import service is not configured")
	}
	for name, value := range map[string]string{
		"source_system": req.SourceSystem,
		"source_id":     req.SourceID,
		"provider":      req.Provider,
		"auth_mode":     req.AuthMode,
	} {
		if strings.TrimSpace(value) == "" {
			return contracts.Account{}, fmt.Errorf("%w: missing %s", contracts.ErrInvalidContract, name)
		}
	}
	if len(req.SourceSystem) > 64 || len(req.SourceID) > 256 || len(req.Provider) > 64 || len(req.AuthMode) > 32 {
		return contracts.Account{}, fmt.Errorf("%w: import identifier is too large", contracts.ErrInvalidContract)
	}
	if req.AuthMode != provider.AuthModeAPIKey && req.AuthMode != provider.AuthModeOAuth && req.AuthMode != "pkce" && req.AuthMode != "device" && req.AuthMode != "cli" {
		return contracts.Account{}, fmt.Errorf("%w: unsupported auth_mode %q", contracts.ErrInvalidContract, req.AuthMode)
	}
	var p provider.Provider
	var ok bool
	if s.Provider == nil {
		p, ok = provider.GetForAuth(req.Provider, req.AuthMode)
	} else {
		p, ok = s.Provider(req.Provider)
	}
	if !ok {
		return contracts.Account{}, fmt.Errorf("%w: %s", ErrProviderMissing, req.Provider)
	}
	bundle, err := p.Authorize(ctx, req)
	if err != nil {
		return contracts.Account{}, err
	}
	if strings.TrimSpace(bundle.AccessToken) == "" && strings.TrimSpace(bundle.RefreshToken) == "" {
		return contracts.Account{}, fmt.Errorf("%w: provider returned an empty token bundle", contracts.ErrInvalidContract)
	}
	accountID := importedAccountID(req.SourceSystem, req.SourceID)
	plaintext, err := json.Marshal(bundle)
	if err != nil {
		return contracts.Account{}, err
	}
	encrypted, err := s.Cipher.Encrypt(accountID, plaintext)
	if err != nil {
		return contracts.Account{}, err
	}
	group := req.Metadata["group"]
	if group == "" {
		group = "default"
	}
	if len(group) > 128 {
		return contracts.Account{}, fmt.Errorf("%w: group is too large", contracts.ErrInvalidContract)
	}
	credentialKind := "oauth"
	if req.AuthMode == provider.AuthModeAPIKey {
		credentialKind = "static"
	}
	account := contracts.Account{
		ID:       accountID,
		Provider: req.Provider,
		Platform: req.Provider,
		Group:    group,
		Status:   "active",
		Credential: contracts.Credential{
			Kind:        credentialKind,
			AccessToken: bundle.AccessToken,
			ExpiresAt:   bundle.ExpiresAt,
		},
		Limits: contracts.AccountLimits{}.WithDefaults(),
	}
	account.Profile = p.Profile(account)
	if profiler, ok := p.(provider.ImportProfileProvider); ok {
		account.Profile, err = profiler.ProfileForImport(account, req)
		if err != nil {
			return contracts.Account{}, err
		}
	}
	if proxy := strings.TrimSpace(req.Metadata["proxy"]); proxy != "" {
		account.Profile.Proxy = proxy
	}
	if req.DryRun {
		return account, nil
	}
	if s.Repository == nil {
		return contracts.Account{}, errors.New("import repository is not configured")
	}
	unlock, acquired, err := s.Repository.TryLock(ctx, accountID)
	if err != nil {
		return contracts.Account{}, err
	}
	if !acquired {
		return contracts.Account{}, ErrRefreshBusy
	}
	released := false
	defer func() {
		if !released {
			_ = unlock()
		}
	}()
	accountID, epoch, credentialVersion, err := s.Repository.Save(ctx, ImportRecord{Request: req, Account: account, EncryptedSecret: encrypted})
	if err != nil {
		return contracts.Account{}, err
	}
	if err := unlock(); err != nil {
		return contracts.Account{}, err
	}
	released = true
	account.ID, account.FenceEpoch, account.Credential.Version = accountID, epoch, credentialVersion
	return account, nil
}

func importedAccountID(sourceSystem, sourceID string) string {
	digest := sha256.Sum256([]byte(sourceSystem + "\x00" + sourceID))
	return "account-" + hex.EncodeToString(digest[:16])
}
