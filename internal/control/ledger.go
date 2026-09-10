package control

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/elucid/gateway-platform/internal/detail"
	"github.com/elucid/gateway-platform/internal/observability"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Ledger struct {
	DB      *pgxpool.Pool
	Metrics *observability.Registry
	// Signals aggregates platform-level transport rejections. Optional.
	Signals *PlatformSignals
	// Detail is A12's request-detail sink. Optional, and deliberately typed as
	// something that cannot fail: it is fed after the metering transaction has
	// committed, and a nil sink is a valid "detail disabled" deployment.
	Detail *detail.Sink
}

func (l Ledger) HandleAttemptStarted(ctx context.Context, event contracts.AttemptStarted) error {
	if l.DB == nil {
		return errors.New("database pool is nil")
	}
	_, err := l.DB.Exec(ctx, `INSERT INTO request_attempts (attempt_id, request_id, started_at, deadline_at, state, account_id, provider, model, tenant_id) VALUES ($1,$2,$3,$4,'open',$5,$6,$7,$8) ON CONFLICT (attempt_id) DO NOTHING`, event.AttemptID, event.RequestID, event.OccurredAt, event.DeadlineAt, event.AccountID, event.Provider, event.Model, event.TenantID)
	return err
}

func (l Ledger) HandleRelease(ctx context.Context, release contracts.Release) error {
	if l.DB == nil {
		return errors.New("database pool is nil")
	}
	tx, err := l.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM request_attempts WHERE attempt_id=$1 FOR UPDATE`, release.AttemptID).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("attempt %s is not persisted yet", release.AttemptID)
		}
		return err
	}
	result, err := tx.Exec(ctx, `INSERT INTO usage_ledger (event_id, attempt_id, request_id, tenant_id, account_id, provider, model, status_code, error_class, tokens_in, tokens_out, cache_read_tokens, cache_write_tokens, usage_source, partial, occurred_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) ON CONFLICT DO NOTHING`, release.EventID, release.AttemptID, release.RequestID, release.TenantID, release.AccountID, release.Provider, release.Model, release.StatusCode, release.ErrorClass, release.TokensIn, release.TokensOut, release.CacheReadTokens, release.CacheWriteTokens, release.UsageSource, release.Partial, release.OccurredAt)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE request_attempts SET terminal_event_id=COALESCE(terminal_event_id,$1), state=CASE WHEN state='reconciled' THEN state ELSE 'terminal' END WHERE attempt_id=$2`, release.EventID, release.AttemptID)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 1 {
		if err := applyReleaseAuthority(ctx, tx, release); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		l.metrics().AddCounter("usage_ledger_duplicate_total", 1)
	} else {
		l.metrics().AddCounter("request_attempt_recovered_total", 1, "source", "gateway")
		l.Signals.Observe(release)
	}
	// A12: request detail is fed here and nowhere else -- after the metering
	// transaction has committed, outside it, and through a call that returns
	// nothing. There is no path by which a detail failure can roll back the
	// ledger or stop the stream ACK below the caller.
	l.Detail.Observe(release)
	return nil
}

// RecoverAttempts closes expired open attempts with a deterministic synthetic release.
func (l Ledger) RecoverAttempts(ctx context.Context, before time.Time) (int, error) {
	if l.DB == nil {
		return 0, errors.New("database pool is nil")
	}
	var openAge float64
	if err := l.DB.QueryRow(ctx, `SELECT COALESCE(EXTRACT(EPOCH FROM (now() - MIN(started_at))),0) FROM request_attempts WHERE state='open'`).Scan(&openAge); err == nil {
		l.metrics().SetGauge("request_attempt_open_age_seconds", openAge)
	}
	tx, err := l.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT attempt_id, request_id, account_id, provider, model, tenant_id FROM request_attempts WHERE state='open' AND deadline_at < $1 FOR UPDATE SKIP LOCKED`, before)
	if err != nil {
		return 0, err
	}
	type candidate struct{ attemptID, requestID, accountID, provider, model, tenantID string }
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.attemptID, &item.requestID, &item.accountID, &item.provider, &item.model, &item.tenantID); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	count := 0
	for _, item := range candidates {
		eventID := contracts.TerminalEventID(item.attemptID)
		release := contracts.Release{SchemaVersion: contracts.SchemaVersion, EventID: eventID, RequestID: item.requestID, AttemptID: item.attemptID, AttemptNo: 0, ProducerID: "control-reconciler", OccurredAt: time.Now().UTC(), AccountID: item.accountID, Provider: item.provider, StatusCode: 504, Model: item.model, TenantID: item.tenantID, ErrorClass: contracts.ErrorNetwork, UsageSource: contracts.UsageSourceMissing, Partial: true}
		_, err = tx.Exec(ctx, `INSERT INTO usage_ledger (event_id, attempt_id, request_id, tenant_id, account_id, provider, model, status_code, error_class, usage_source, partial, occurred_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT DO NOTHING`, release.EventID, release.AttemptID, release.RequestID, release.TenantID, release.AccountID, release.Provider, release.Model, release.StatusCode, release.ErrorClass, release.UsageSource, release.Partial, release.OccurredAt)
		if err != nil {
			return count, err
		}
		result, err := tx.Exec(ctx, `UPDATE request_attempts SET terminal_event_id=$1, state='reconciled', reconciled_at=now() WHERE attempt_id=$2 AND state='open'`, eventID, item.attemptID)
		if err != nil {
			return count, err
		}
		count += int(result.RowsAffected())
	}
	return count, tx.Commit(ctx)
}

func (l Ledger) metrics() *observability.Registry {
	if l.Metrics != nil {
		return l.Metrics
	}
	return observability.Default
}
