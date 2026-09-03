package control

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/elucid/gateway-platform/internal/control/credentials"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	refreshBatchSize = 100
	refreshAhead     = 5 * time.Minute
)

type RefreshLoop struct {
	Repository   PGCredentialRepository
	Service      RefreshService
	QuotaService QuotaService
	Now          func() time.Time
}

func NewRefreshLoop(db *pgxpool.Pool, encodedKey string) (*RefreshLoop, error) {
	key, err := base64.StdEncoding.Strict().DecodeString(encodedKey)
	if err != nil {
		return nil, errors.New("GATEWAY_CREDENTIAL_KEY must be valid base64")
	}
	cipher, err := credentials.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("GATEWAY_CREDENTIAL_KEY: %w", err)
	}
	repository := PGCredentialRepository{DB: db}
	return &RefreshLoop{
		Repository:   repository,
		Service:      RefreshService{Repository: repository, Cipher: cipher},
		QuotaService: QuotaService{Repository: repository, Cipher: cipher},
		Now:          time.Now,
	}, nil
}

func (l RefreshLoop) RunOnce(ctx context.Context) error {
	now := l.Now
	if now == nil {
		now = time.Now
	}
	rows, err := l.Repository.DB.Query(ctx, `SELECT a.id FROM accounts a JOIN credentials c ON c.account_id=a.id WHERE a.status='active' AND c.kind='oauth' AND c.expires_at IS NOT NULL AND c.expires_at <= $1 ORDER BY c.expires_at ASC LIMIT $2`, now().Add(refreshAhead), refreshBatchSize)
	if err != nil {
		return err
	}
	var accountIDs []string
	for rows.Next() {
		var accountID string
		if err := rows.Scan(&accountID); err != nil {
			rows.Close()
			return err
		}
		accountIDs = append(accountIDs, accountID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	var refreshErr error
	for _, accountID := range accountIDs {
		if _, err := l.Service.Refresh(ctx, accountID); err != nil && !errors.Is(err, ErrRefreshBusy) {
			refreshErr = errors.Join(refreshErr, fmt.Errorf("refresh account %s: %w", accountID, err))
		}
	}
	return refreshErr
}
