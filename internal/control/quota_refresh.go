package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/elucid/gateway-platform/internal/control/credentials"
	"github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/internal/observability"
	"github.com/elucid/gateway-platform/internal/snapshot"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

const (
	quotaBatchSize         = 100
	quotaReconcileInterval = time.Minute
	quotaOutboxLease       = quotaReconcileInterval
	quotaOutboxMinBackoff  = quotaReconcileInterval
	quotaOutboxMaxBackoff  = 15 * time.Minute
)

// QuotaRepository is an optional extension of CredentialRepository. Keeping
// it separate preserves existing refresh/import test doubles and contracts.
type QuotaRepository interface {
	CredentialRepository
	CommitQuota(context.Context, StoredCredential, contracts.QuotaInfo) error
}

type QuotaOutboxItem struct {
	AccountID  string
	Platform   string
	Group      string
	FenceEpoch int64
	Quota      contracts.QuotaInfo
	Attempts   int
}

type QuotaOutboxRepository interface {
	QuotaRepository
	CommitQuotaAndQueue(context.Context, StoredCredential, contracts.QuotaInfo) error
	ClaimQuotaOutbox(context.Context, int) ([]QuotaOutboxItem, error)
	AckQuotaOutbox(context.Context, QuotaOutboxItem) error
	RetryQuotaOutbox(context.Context, QuotaOutboxItem, error, time.Time) error
}

type QuotaFenceRepository interface {
	QuotaOutboxRepository
	CurrentFence(context.Context, string) (int64, error)
}

type QuotaSnapshotPublisher interface {
	PublishQuota(context.Context, StoredCredential, contracts.QuotaInfo) error
}

// QuotaService fetches quota under the same account lock and fence epoch used
// by OAuth refresh. A missing adapter response is intentionally a no-op so an
// unverified provider cannot erase a previously published quota snapshot.
type QuotaService struct {
	Repository CredentialRepository
	Cipher     *credentials.Cipher
	Provider   func(string) (provider.Provider, bool)
	Snapshot   QuotaSnapshotPublisher
}

func (s QuotaService) Refresh(ctx context.Context, accountID string) (contracts.QuotaInfo, error) {
	quotaRepository, ok := s.Repository.(QuotaRepository)
	if !ok {
		return contracts.QuotaInfo{}, errors.New("quota repository is not configured")
	}
	if s.Cipher == nil {
		return contracts.QuotaInfo{}, errors.New("quota service is not configured")
	}
	unlock, acquired, err := s.Repository.TryLock(ctx, accountID)
	if err != nil {
		return contracts.QuotaInfo{}, err
	}
	if !acquired {
		return contracts.QuotaInfo{}, ErrRefreshBusy
	}
	released := false
	defer func() {
		if !released {
			_ = unlock()
		}
	}()
	stored, err := s.Repository.Acquire(ctx, accountID)
	if err != nil {
		return contracts.QuotaInfo{}, err
	}
	current, err := decryptTokenBundle(s.Cipher, stored)
	if err != nil {
		return contracts.QuotaInfo{}, err
	}
	var p provider.Provider
	if s.Provider == nil {
		p, ok = provider.GetForAuth(stored.Provider, stored.CredentialKind)
	} else {
		p, ok = s.Provider(stored.Provider)
	}
	if !ok {
		return contracts.QuotaInfo{}, fmt.Errorf("%w: %s", ErrProviderMissing, stored.Provider)
	}
	quotaCredential := contracts.Credential{Kind: stored.CredentialKind, AccessToken: current.AccessToken, Proxy: stored.Proxy, Metadata: current.Metadata, ExpiresAt: current.ExpiresAt}
	quota, err := p.Quota(ctx, quotaCredential)
	if err != nil {
		if failureErr := s.Repository.Fail(ctx, stored, provider.ErrorClass(err)); failureErr != nil {
			return contracts.QuotaInfo{}, errors.Join(err, failureErr)
		}
		return contracts.QuotaInfo{}, err
	}
	// A nil Items slice is the adapter's explicit "not configured/missing"
	// state. Preserve the last known quota in that case.
	if quota.Items == nil {
		if err := unlock(); err != nil {
			return contracts.QuotaInfo{}, err
		}
		released = true
		return quota, nil
	}
	outboxRepository, hasOutbox := s.Repository.(QuotaOutboxRepository)
	if hasOutbox {
		err = outboxRepository.CommitQuotaAndQueue(ctx, stored, quota)
	} else {
		err = quotaRepository.CommitQuota(ctx, stored, quota)
	}
	if err != nil {
		return contracts.QuotaInfo{}, err
	}
	if s.Snapshot != nil {
		if err := s.Snapshot.PublishQuota(ctx, stored, quota); err != nil {
			return contracts.QuotaInfo{}, err
		}
		if hasOutbox {
			if err := outboxRepository.AckQuotaOutbox(ctx, QuotaOutboxItem{AccountID: stored.AccountID, FenceEpoch: stored.FenceEpoch}); err != nil {
				return contracts.QuotaInfo{}, err
			}
		}
	}
	if err := unlock(); err != nil {
		return contracts.QuotaInfo{}, err
	}
	released = true
	return quota, nil
}

func decryptTokenBundle(cipher *credentials.Cipher, stored StoredCredential) (contracts.TokenBundle, error) {
	plaintext, err := cipher.Decrypt(stored.AccountID, stored.EncryptedSecret)
	if err != nil {
		return contracts.TokenBundle{}, fmt.Errorf("decrypt credential: %w", err)
	}
	var current contracts.TokenBundle
	if err := json.Unmarshal(plaintext, &current); err != nil {
		return contracts.TokenBundle{}, errors.New("decode encrypted credential: invalid token bundle")
	}
	return current, nil
}

func (r PGCredentialRepository) CommitQuota(ctx context.Context, stored StoredCredential, quota contracts.QuotaInfo) error {
	if r.DB == nil {
		return errors.New("database pool is nil")
	}
	payload, err := marshalQuota(quota)
	if err != nil {
		return err
	}
	result, err := r.DB.Exec(ctx, `UPDATE accounts SET quota=$1::jsonb, updated_at=now() WHERE id=$2 AND fence_epoch=$3`, string(payload), stored.AccountID, stored.FenceEpoch)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrFenceLost
	}
	return nil
}

func (r PGCredentialRepository) CommitQuotaAndQueue(ctx context.Context, stored StoredCredential, quota contracts.QuotaInfo) error {
	if r.DB == nil {
		return errors.New("database pool is nil")
	}
	payload, err := marshalQuota(quota)
	if err != nil {
		return err
	}
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE accounts SET quota=$1::jsonb, updated_at=now() WHERE id=$2 AND fence_epoch=$3`, string(payload), stored.AccountID, stored.FenceEpoch)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrFenceLost
	}
	if _, err := tx.Exec(ctx, `INSERT INTO quota_snapshot_outbox (account_id,platform,"group",fence_epoch,quota,next_attempt_at,locked_until,last_error,updated_at) VALUES ($1,$2,$3,$4,$5::jsonb,now(),NULL,'',now()) ON CONFLICT (account_id) DO UPDATE SET platform=EXCLUDED.platform,"group"=EXCLUDED."group",fence_epoch=EXCLUDED.fence_epoch,quota=EXCLUDED.quota,attempts=0,next_attempt_at=now(),locked_until=NULL,last_error='',updated_at=now()`, stored.AccountID, stored.Platform, stored.Group, stored.FenceEpoch, string(payload)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func marshalQuota(quota contracts.QuotaInfo) ([]byte, error) {
	return json.Marshal(quota)
}

func (r PGCredentialRepository) CurrentFence(ctx context.Context, accountID string) (int64, error) {
	if r.DB == nil {
		return 0, errors.New("database pool is nil")
	}
	var epoch int64
	if err := r.DB.QueryRow(ctx, `SELECT fence_epoch FROM accounts WHERE id=$1`, accountID).Scan(&epoch); errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrFenceLost
	} else if err != nil {
		return 0, err
	}
	return epoch, nil
}

func (r PGCredentialRepository) ClaimQuotaOutbox(ctx context.Context, limit int) ([]QuotaOutboxItem, error) {
	if r.DB == nil {
		return nil, errors.New("database pool is nil")
	}
	if limit <= 0 {
		return nil, fmt.Errorf("%w: quota outbox limit must be positive", contracts.ErrInvalidContract)
	}
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT account_id,platform,"group",fence_epoch,quota,attempts FROM quota_snapshot_outbox WHERE next_attempt_at <= now() AND (locked_until IS NULL OR locked_until <= now()) ORDER BY next_attempt_at,updated_at LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, err
	}
	items := make([]QuotaOutboxItem, 0)
	for rows.Next() {
		var item QuotaOutboxItem
		var payload []byte
		if err := rows.Scan(&item.AccountID, &item.Platform, &item.Group, &item.FenceEpoch, &payload, &item.Attempts); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal(payload, &item.Quota); err != nil {
			rows.Close()
			return nil, fmt.Errorf("decode quota outbox account %s: %w", item.AccountID, err)
		}
		item.Attempts++
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, item := range items {
		if _, err := tx.Exec(ctx, `UPDATE quota_snapshot_outbox SET attempts=attempts+1,locked_until=now()+$1 * interval '1 second',updated_at=now() WHERE account_id=$2 AND fence_epoch=$3`, int64(quotaOutboxLease/time.Second), item.AccountID, item.FenceEpoch); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return items, nil
}

func (r PGCredentialRepository) AckQuotaOutbox(ctx context.Context, item QuotaOutboxItem) error {
	if r.DB == nil {
		return errors.New("database pool is nil")
	}
	result, err := r.DB.Exec(ctx, `DELETE FROM quota_snapshot_outbox WHERE account_id=$1 AND fence_epoch=$2`, item.AccountID, item.FenceEpoch)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrFenceLost
	}
	return nil
}

func (r PGCredentialRepository) RetryQuotaOutbox(ctx context.Context, item QuotaOutboxItem, cause error, nextAttempt time.Time) error {
	if r.DB == nil {
		return errors.New("database pool is nil")
	}
	result, err := r.DB.Exec(ctx, `UPDATE quota_snapshot_outbox SET next_attempt_at=$1,locked_until=NULL,last_error=$2,updated_at=now() WHERE account_id=$3 AND fence_epoch=$4`, nextAttempt, quotaOutboxErrorState(cause), item.AccountID, item.FenceEpoch)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrFenceLost
	}
	return nil
}

func quotaOutboxErrorState(cause error) string {
	switch {
	case errors.Is(cause, context.Canceled):
		return "canceled"
	case errors.Is(cause, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(cause, ErrRefreshBusy):
		return "busy"
	case errors.Is(cause, ErrFenceLost):
		return "fence_lost"
	case errors.Is(cause, snapshot.ErrStaleEpoch):
		return "stale_epoch"
	default:
		return "snapshot_publish_failed"
	}
}

// RedisQuotaSnapshotPublisher updates only the account quota in the current
// complete bucket, then atomically publishes the next snapshot epoch.
type RedisQuotaSnapshotPublisher struct {
	Redis redis.UniversalClient
}

func (p RedisQuotaSnapshotPublisher) PublishQuota(ctx context.Context, stored StoredCredential, quota contracts.QuotaInfo) error {
	if p.Redis == nil {
		return errors.New("redis client is nil")
	}
	if stored.Platform == "" || stored.Group == "" {
		return fmt.Errorf("%w: quota snapshot bucket is missing", contracts.ErrInvalidContract)
	}
	current, err := (snapshot.Loader{Redis: p.Redis}).Load(ctx, stored.Platform, stored.Group)
	if err != nil {
		return fmt.Errorf("load quota snapshot: %w", err)
	}
	found := false
	for i := range current.Accounts {
		if current.Accounts[i].ID != stored.AccountID {
			continue
		}
		if reflect.DeepEqual(current.Accounts[i].Quota, quota) {
			return nil
		}
		current.Accounts[i].Quota = quota
		found = true
		break
	}
	if !found {
		return fmt.Errorf("%w: account %s is missing from snapshot", contracts.ErrInvalidContract, stored.AccountID)
	}
	if _, err := (snapshot.Publisher{Redis: p.Redis}).Publish(ctx, stored.Platform, stored.Group, current.Epoch, current.Accounts); err != nil {
		return fmt.Errorf("publish quota snapshot: %w", err)
	}
	return nil
}

type QuotaLoop struct {
	Repository PGCredentialRepository
	Service    QuotaService
	List       func(context.Context, int) ([]string, error)
}

func (l QuotaLoop) RunOnce(ctx context.Context) error {
	list := l.List
	if list == nil {
		list = func(ctx context.Context, limit int) ([]string, error) {
			if l.Repository.DB == nil {
				return nil, errors.New("database pool is nil")
			}
			rows, err := l.Repository.DB.Query(ctx, `SELECT a.id FROM accounts a JOIN credentials c ON c.account_id=a.id WHERE a.status='active' AND a.provider IN ('codex','claude') ORDER BY a.updated_at ASC LIMIT $1`, limit)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var accountIDs []string
			for rows.Next() {
				var accountID string
				if err := rows.Scan(&accountID); err != nil {
					return nil, err
				}
				accountIDs = append(accountIDs, accountID)
			}
			return accountIDs, rows.Err()
		}
	}
	accountIDs, err := list(ctx, quotaBatchSize)
	if err != nil {
		return err
	}
	var refreshErr error
	for _, accountID := range accountIDs {
		if _, err := l.Service.Refresh(ctx, accountID); err != nil && !errors.Is(err, ErrRefreshBusy) {
			refreshErr = errors.Join(refreshErr, fmt.Errorf("quota account %s: %w", accountID, err))
		}
	}
	return refreshErr
}

type QuotaReconciler struct {
	Repository QuotaFenceRepository
	Snapshot   QuotaSnapshotPublisher
	Metrics    *observability.Registry
	Now        func() time.Time
}

func (r QuotaReconciler) RunOnce(ctx context.Context) error {
	if r.Repository == nil || r.Snapshot == nil {
		return errors.New("quota reconciler is not configured")
	}
	now := r.Now
	if now == nil {
		now = time.Now
	}
	items, err := r.Repository.ClaimQuotaOutbox(ctx, quotaBatchSize)
	if err != nil {
		return err
	}
	var reconcileErr error
	for _, item := range items {
		if err := r.reconcileItem(ctx, item, now()); err != nil {
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("quota snapshot account %s: %w", item.AccountID, err))
		}
	}
	return reconcileErr
}

func (r QuotaReconciler) reconcileItem(ctx context.Context, item QuotaOutboxItem, currentTime time.Time) error {
	unlock, acquired, err := r.Repository.TryLock(ctx, item.AccountID)
	if err != nil {
		return r.retry(ctx, item, err, currentTime)
	}
	if !acquired {
		r.metric("busy")
		return r.retry(ctx, item, ErrRefreshBusy, currentTime)
	}
	defer unlock()
	currentFence, err := r.Repository.CurrentFence(ctx, item.AccountID)
	if err != nil {
		return r.retry(ctx, item, err, currentTime)
	}
	if currentFence != item.FenceEpoch {
		if err := r.Repository.AckQuotaOutbox(ctx, item); err != nil {
			return err
		}
		r.metric("stale")
		return nil
	}
	stored := StoredCredential{AccountID: item.AccountID, Platform: item.Platform, Group: item.Group, FenceEpoch: item.FenceEpoch}
	if err := r.Snapshot.PublishQuota(ctx, stored, item.Quota); err != nil {
		return r.retry(ctx, item, err, currentTime)
	}
	if err := r.Repository.AckQuotaOutbox(ctx, item); err != nil {
		return r.retry(ctx, item, err, currentTime)
	}
	r.metric("published")
	return nil
}

func (r QuotaReconciler) retry(ctx context.Context, item QuotaOutboxItem, cause error, currentTime time.Time) error {
	if err := ctx.Err(); err != nil {
		r.metric("canceled")
		return err
	}
	next := currentTime.Add(quotaOutboxBackoff(item.Attempts))
	if err := r.Repository.RetryQuotaOutbox(ctx, item, cause, next); err != nil {
		return errors.Join(cause, err)
	}
	r.metric("retry")
	return cause
}

func quotaOutboxBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	delay := quotaOutboxMinBackoff
	for i := 1; i < attempts && delay < quotaOutboxMaxBackoff; i++ {
		delay *= 2
	}
	if delay > quotaOutboxMaxBackoff {
		return quotaOutboxMaxBackoff
	}
	return delay
}

func (r QuotaReconciler) metric(result string) {
	metrics := r.Metrics
	if metrics == nil {
		metrics = observability.Default
	}
	metrics.AddCounter("quota_snapshot_reconcile_total", 1, "result", strings.TrimSpace(result))
}
