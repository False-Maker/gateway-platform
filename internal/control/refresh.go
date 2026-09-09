package control

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/elucid/gateway-platform/internal/control/credentials"
	"github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrRefreshBusy     = errors.New("account refresh is already running")
	ErrProviderMissing = errors.New("credential provider is not registered")
)

type StoredCredential struct {
	AccountID       string
	Provider        string
	Platform        string
	Group           string
	Proxy           string
	CredentialKind  string
	FenceEpoch      int64
	EncryptedSecret []byte
}

type CredentialRepository interface {
	TryLock(context.Context, string) (func() error, bool, error)
	Acquire(context.Context, string) (StoredCredential, error)
	Commit(context.Context, StoredCredential, []byte, *contracts.TokenBundle) error
	Fail(context.Context, StoredCredential, contracts.ErrorClass) error
}

type PGCredentialRepository struct {
	DB *pgxpool.Pool
}

func (r PGCredentialRepository) TryLock(ctx context.Context, accountID string) (func() error, bool, error) {
	if strings.TrimSpace(accountID) == "" {
		return nil, false, fmt.Errorf("%w: account_id is empty", contracts.ErrInvalidContract)
	}
	return (Fencer{DB: r.DB}).TryAcquireSingleton(ctx, accountLockKey(accountID))
}

// Acquire increments fence_epoch before reading the encrypted credential.
// TryLock must be held by the caller for the complete refresh operation.
func (r PGCredentialRepository) Acquire(ctx context.Context, accountID string) (StoredCredential, error) {
	epoch, err := (Fencer{DB: r.DB}).Acquire(ctx, accountID)
	if err != nil {
		return StoredCredential{}, err
	}
	var stored StoredCredential
	stored.AccountID, stored.FenceEpoch = accountID, epoch
	err = r.DB.QueryRow(ctx, `SELECT a.provider,a.platform,a."group",a.proxy,c.kind,c.encrypted_secret FROM accounts a JOIN credentials c ON c.account_id=a.id WHERE a.id=$1 AND a.fence_epoch=$2`, accountID, epoch).Scan(&stored.Provider, &stored.Platform, &stored.Group, &stored.Proxy, &stored.CredentialKind, &stored.EncryptedSecret)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredCredential{}, ErrFenceLost
	}
	return stored, err
}

func (r PGCredentialRepository) Commit(ctx context.Context, stored StoredCredential, encrypted []byte, bundle *contracts.TokenBundle) error {
	if bundle == nil {
		return fmt.Errorf("%w: refreshed token bundle is nil", contracts.ErrInvalidContract)
	}
	expiresAt := bundle.ExpiresAt
	if expiresAt.IsZero() {
		return (Fencer{DB: r.DB}).UpdateCredential(ctx, stored.AccountID, stored.FenceEpoch, encrypted, nil)
	}
	return (Fencer{DB: r.DB}).UpdateCredential(ctx, stored.AccountID, stored.FenceEpoch, encrypted, &expiresAt)
}

func (r PGCredentialRepository) Fail(ctx context.Context, stored StoredCredential, class contracts.ErrorClass) error {
	if !class.Valid() || class == contracts.ErrorOK {
		return fmt.Errorf("%w: invalid refresh error class", contracts.ErrInvalidContract)
	}
	result, err := r.DB.Exec(ctx, `UPDATE accounts SET status=CASE WHEN $1::text='auth_invalid' THEN 'disabled' ELSE status END, last_error_class=$1, last_error_at=now(), updated_at=now() WHERE id=$2 AND fence_epoch=$3`, class, stored.AccountID, stored.FenceEpoch)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrFenceLost
	}
	return nil
}

type RefreshService struct {
	Repository CredentialRepository
	Cipher     *credentials.Cipher
	Provider   func(string) (provider.Provider, bool)
}

func (s RefreshService) Refresh(ctx context.Context, accountID string) (contracts.TokenBundle, error) {
	if s.Repository == nil || s.Cipher == nil {
		return contracts.TokenBundle{}, errors.New("refresh service is not configured")
	}
	unlock, acquired, err := s.Repository.TryLock(ctx, accountID)
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	if !acquired {
		return contracts.TokenBundle{}, ErrRefreshBusy
	}
	released := false
	defer func() {
		if !released {
			_ = unlock()
		}
	}()
	stored, err := s.Repository.Acquire(ctx, accountID)
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	plaintext, err := s.Cipher.Decrypt(accountID, stored.EncryptedSecret)
	if err != nil {
		return contracts.TokenBundle{}, fmt.Errorf("decrypt credential: %w", err)
	}
	var current contracts.TokenBundle
	if err := json.Unmarshal(plaintext, &current); err != nil {
		return contracts.TokenBundle{}, errors.New("decode encrypted credential: invalid token bundle")
	}
	if stored.Proxy != "" {
		if current.Metadata == nil {
			current.Metadata = make(map[string]string)
		}
		if current.Metadata["proxy"] == "" {
			current.Metadata["proxy"] = stored.Proxy
		}
	}
	var p provider.Provider
	var ok bool
	if s.Provider == nil {
		p, ok = provider.GetForAuth(stored.Provider, stored.CredentialKind)
	} else {
		p, ok = s.Provider(stored.Provider)
	}
	if !ok {
		return contracts.TokenBundle{}, fmt.Errorf("%w: %s", ErrProviderMissing, stored.Provider)
	}
	refreshed, err := provider.RefreshOAuth(ctx, p, current)
	if err != nil {
		if failureErr := s.Repository.Fail(ctx, stored, provider.ErrorClass(err)); failureErr != nil {
			return contracts.TokenBundle{}, errors.Join(err, failureErr)
		}
		return contracts.TokenBundle{}, err
	}
	if strings.TrimSpace(refreshed.AccessToken) == "" || strings.TrimSpace(refreshed.RefreshToken) == "" {
		return contracts.TokenBundle{}, fmt.Errorf("%w: refreshed token bundle is incomplete", contracts.ErrInvalidContract)
	}
	plaintext, err = json.Marshal(refreshed)
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	encrypted, err := s.Cipher.Encrypt(accountID, plaintext)
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	if err := s.Repository.Commit(ctx, stored, encrypted, &refreshed); err != nil {
		return contracts.TokenBundle{}, err
	}
	if err := unlock(); err != nil {
		return contracts.TokenBundle{}, err
	}
	released = true
	return refreshed, nil
}

func accountLockKey(accountID string) int64 {
	digest := sha256.Sum256([]byte("gateway-platform/refresh/" + accountID))
	return int64(binary.BigEndian.Uint64(digest[:8]))
}
