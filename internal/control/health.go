package control

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/elucid/gateway-platform/internal/observability"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

// Authoritative ErrorClass handling (docs/fluxgate-keyhive-overview.md §6.3.1,
// "control 持久处理" column). Gateway's local cooldowns only bridge the
// snapshot delay; the durations here are the real verdicts.
const (
	// unknownRateLimitEscalation is how many consecutive header-less 429s an
	// account must return before control imposes a persistent cooldown.
	unknownRateLimitEscalation = 3
	unknownRateLimitCooldown   = 5 * time.Minute
	blockedCooldown            = 60 * time.Second

	// platformSignalWindow is the sliding window over which transport-level
	// rejections are aggregated per platform.
	platformSignalWindow = 60 * time.Second
	// platformSignalAccounts is the number of distinct accounts on one
	// platform that must hit forbidden_transport/blocked inside the window
	// before it is treated as an egress/TLS problem rather than account noise.
	platformSignalAccounts = 2
	// starvationRatio is the share of a platform's active accounts allowed
	// to sit in cooldown before the oldest cooldown is released early.
	starvationRatio = 0.5
)

// applyReleaseAuthority is the single SQL statement that turns a terminal
// Release into the account's persistent state. It runs inside the ledger
// transaction and only for the first delivery of an event.
//
//   ok                    -> clear last_error, consecutive_failures, cooldown
//   auth_invalid          -> status='disabled' (permanent, needs re-auth)
//   forbidden_capability  -> excluded_models += model (per-model, no cooldown)
//   rate_limited_unknown  -> consecutive_failures+1; at threshold -> 5min cooldown
//   blocked               -> consecutive_failures+1, 60s cooldown
//   forbidden_transport, upstream_5xx, network_error, auth_expired,
//   rate_limited_known    -> last_error only; no account penalty
const applyReleaseAuthoritySQL = `
UPDATE accounts SET
  status = CASE WHEN $1::text = 'auth_invalid' THEN 'disabled' ELSE status END,
  last_error_class = CASE WHEN $1::text = 'ok' THEN NULL ELSE $1::text END,
  last_error_at    = CASE WHEN $1::text = 'ok' THEN NULL ELSE $2::timestamptz END,
  consecutive_failures = CASE
      WHEN $1::text = 'ok' THEN 0
      WHEN $1::text IN ('rate_limited_unknown','blocked') THEN consecutive_failures + 1
      ELSE consecutive_failures END,
  cooldown_until = CASE
      WHEN $1::text = 'ok' THEN NULL
      WHEN $1::text = 'blocked' THEN GREATEST(COALESCE(cooldown_until, $2::timestamptz), $2::timestamptz + ($5::int * interval '1 second'))
      WHEN $1::text = 'rate_limited_unknown' AND consecutive_failures + 1 >= $6::int
           THEN GREATEST(COALESCE(cooldown_until, $2::timestamptz), $2::timestamptz + ($4::int * interval '1 second'))
      ELSE cooldown_until END,
  excluded_models = CASE
      WHEN $1::text = 'forbidden_capability' AND $3::text <> '' AND NOT (excluded_models ? $3::text)
           THEN excluded_models || to_jsonb(ARRAY[$3::text])
      ELSE excluded_models END,
  updated_at = now()
WHERE id = $7`

func applyReleaseAuthority(ctx context.Context, tx pgx.Tx, release contracts.Release) error {
	_, err := tx.Exec(ctx, applyReleaseAuthoritySQL,
		string(release.ErrorClass), release.OccurredAt, release.Model,
		int(unknownRateLimitCooldown/time.Second), int(blockedCooldown/time.Second), unknownRateLimitEscalation,
		release.AccountID)
	return err
}

// PlatformSignals aggregates transport-level rejections per platform. A single
// forbidden_transport or blocked never penalises an account; several accounts
// failing the same way inside one window is the only detector this system has
// for a burned egress IP or an outdated TLS fingerprint (overview §6.3.1 rule 2).
type PlatformSignals struct {
	Metrics *observability.Registry
	Now     func() time.Time
	Window  time.Duration
	// Threshold is the distinct-account count that raises the alert gauge.
	Threshold int

	mu     sync.Mutex
	events map[string][]platformEvent // provider -> events inside window
}

type platformEvent struct {
	at        time.Time
	accountID string
	class     contracts.ErrorClass
}

func NewPlatformSignals(metrics *observability.Registry) *PlatformSignals {
	return &PlatformSignals{Metrics: metrics, Window: platformSignalWindow, Threshold: platformSignalAccounts, events: make(map[string][]platformEvent)}
}

func (p *PlatformSignals) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *PlatformSignals) metrics() *observability.Registry {
	if p.Metrics != nil {
		return p.Metrics
	}
	return observability.Default
}

// Observe records one terminal Release and refreshes the platform gauges.
// It returns true when the platform crossed the alert threshold on this call.
func (p *PlatformSignals) Observe(release contracts.Release) bool {
	if p == nil {
		return false
	}
	class := release.ErrorClass
	if class != contracts.ErrorForbiddenTransport && class != contracts.ErrorBlocked && class != contracts.ErrorUpstream5xx {
		return false
	}
	p.metrics().AddCounter("control_platform_error_total", 1, "provider", release.Provider, "class", string(class))
	if class == contracts.ErrorUpstream5xx {
		// 5xx feeds the platform breaker counter only; it is not a
		// fingerprint/egress signal.
		return false
	}
	window := p.Window
	if window <= 0 {
		window = platformSignalWindow
	}
	threshold := p.Threshold
	if threshold <= 0 {
		threshold = platformSignalAccounts
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.events == nil {
		p.events = make(map[string][]platformEvent)
	}
	kept := p.events[release.Provider][:0]
	for _, event := range p.events[release.Provider] {
		if now.Sub(event.at) < window {
			kept = append(kept, event)
		}
	}
	kept = append(kept, platformEvent{at: now, accountID: release.AccountID, class: class})
	p.events[release.Provider] = kept
	distinct := make(map[string]struct{}, len(kept))
	for _, event := range kept {
		distinct[event.accountID] = struct{}{}
	}
	alerting := len(distinct) >= threshold
	p.metrics().SetGauge("control_platform_transport_rejections_accounts", float64(len(distinct)), "provider", release.Provider)
	value := 0.0
	if alerting {
		value = 1
	}
	p.metrics().SetGauge("control_platform_alert", value, "provider", release.Provider, "reason", "transport_rejections")
	return alerting
}

// Alerting reports the providers currently over threshold, for tests and
// operators; it does not mutate the window.
func (p *PlatformSignals) Alerting() []string {
	if p == nil {
		return nil
	}
	window := p.Window
	if window <= 0 {
		window = platformSignalWindow
	}
	threshold := p.Threshold
	if threshold <= 0 {
		threshold = platformSignalAccounts
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for provider, events := range p.events {
		distinct := make(map[string]struct{})
		for _, event := range events {
			if now.Sub(event.at) < window {
				distinct[event.accountID] = struct{}{}
			}
		}
		if len(distinct) >= threshold {
			out = append(out, provider)
		}
	}
	sort.Strings(out)
	return out
}

// StarvationGuard is the optional pool-starvation gate (overview §6.3.1 rule 3):
// when more than half of a platform's active accounts are cooling, the oldest
// cooldown is released early because the failure is almost certainly
// platform-wide, not per-account.
type StarvationGuard interface {
	ReleaseStarvedCooldowns(ctx context.Context, now time.Time) ([]string, error)
}

// ReleaseStarvedCooldowns implements StarvationGuard on PostgreSQL. It returns
// the platforms on which a cooldown was released.
func (r PGSnapshotRepository) ReleaseStarvedCooldowns(ctx context.Context, now time.Time) ([]string, error) {
	if r.DB == nil {
		return nil, errors.New("snapshot database pool is nil")
	}
	rows, err := r.DB.Query(ctx, `
		WITH per_platform AS (
			SELECT platform,
			       count(*) FILTER (WHERE cooldown_until IS NOT NULL AND cooldown_until > $1) AS cooling,
			       count(*) AS total
			FROM accounts WHERE status='active' GROUP BY platform
		), starved AS (
			SELECT platform FROM per_platform WHERE total > 0 AND cooling::float / total::float > $2
		), victim AS (
			SELECT DISTINCT ON (a.platform) a.id, a.platform
			FROM accounts a JOIN starved s ON s.platform = a.platform
			WHERE a.status='active' AND a.cooldown_until IS NOT NULL AND a.cooldown_until > $1
			ORDER BY a.platform, a.cooldown_until ASC
		)
		UPDATE accounts SET cooldown_until = NULL, consecutive_failures = 0, updated_at = now()
		FROM victim WHERE accounts.id = victim.id
		RETURNING accounts.platform`, now, starvationRatio)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var platforms []string
	for rows.Next() {
		var platform string
		if err := rows.Scan(&platform); err != nil {
			return nil, err
		}
		platforms = append(platforms, platform)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(platforms)
	for _, platform := range platforms {
		observability.Default.AddCounter("control_platform_starvation_release_total", 1, "platform", platform)
	}
	return platforms, nil
}

var _ StarvationGuard = PGSnapshotRepository{}
