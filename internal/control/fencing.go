package control

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrFenceLost = errors.New("account fencing token lost")

type Fencer struct{ DB *pgxpool.Pool }

// Acquire increments the account epoch before a credential is read or refreshed.
func (f Fencer) Acquire(ctx context.Context, accountID string) (int64, error) {
	if f.DB == nil {
		return 0, errors.New("database pool is nil")
	}
	var epoch int64
	err := f.DB.QueryRow(ctx, `UPDATE accounts SET fence_epoch=fence_epoch+1, updated_at=now() WHERE id=$1 RETURNING fence_epoch`, accountID).Scan(&epoch)
	return epoch, err
}

func (f Fencer) UpdateCredential(ctx context.Context, accountID string, epoch int64, encryptedSecret []byte, expiresAt *time.Time) error {
	if f.DB == nil {
		return errors.New("database pool is nil")
	}
	tx, err := f.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE credentials SET encrypted_secret=$1, expires_at=$2, version=version+1, updated_at=now() WHERE account_id=$3 AND EXISTS (SELECT 1 FROM accounts WHERE id=$3 AND fence_epoch=$4)`, encryptedSecret, expiresAt, accountID, epoch)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrFenceLost
	}
	accountResult, err := tx.Exec(ctx, `UPDATE accounts SET updated_at=now() WHERE id=$1 AND fence_epoch=$2`, accountID, epoch)
	if err != nil {
		return err
	}
	if accountResult.RowsAffected() != 1 {
		return ErrFenceLost
	}
	return tx.Commit(ctx)
}

func (f Fencer) TryAcquireSingleton(ctx context.Context, key int64) (func() error, bool, error) {
	if f.DB == nil {
		return nil, false, errors.New("database pool is nil")
	}
	conn, err := f.DB.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	var once sync.Once
	var unlockErr error
	return func() error {
		once.Do(func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var unlocked bool
			unlockErr = conn.QueryRow(unlockCtx, `SELECT pg_advisory_unlock($1)`, key).Scan(&unlocked)
			if unlockErr == nil && unlocked {
				conn.Release()
				return
			}
			if unlockErr == nil {
				unlockErr = errors.New("advisory lock was not held")
			}
			raw := conn.Hijack()
			_ = raw.Close(context.Background())
		})
		return unlockErr
	}, true, nil
}
