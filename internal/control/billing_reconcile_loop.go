package control

import (
	"context"
	"fmt"
	"time"

	"github.com/elucid/gateway-platform/internal/observability"
)

// reconcileRunInterval paces the scheduled reconciliation. Like the deduction
// job it is deliberately slow: reconciliation is read-only and never on a
// request's path, so a longer interval only delays a finding.
const reconcileRunInterval = 15 * time.Minute

// reconcileWindow is how far back each scheduled run looks.
//
// It is wider than the interval on purpose. Runs must overlap: a window that
// exactly tiled the interval would drop any row whose occurred_at landed
// between the last query and the tick, and a reconciliation that can miss rows
// is worse than none -- it reports "balanced" over a gap it never read.
// Re-examining the same row is free because the run writes nothing.
const reconcileWindow = time.Hour

// reconcileLag holds the window back from now.
//
// B4.2 settles rows minutes after they occur, so the most recent stretch of
// usage_ledger is legitimately full of `pending` rows that no wallet movement
// matches yet. Reconciling right up to now would therefore report a difference
// on every single run, and an alert that always fires is an alert nobody
// reads. The lag must exceed billingRunInterval; the check below enforces it
// rather than trusting the two constants to be edited together.
const reconcileLag = 15 * time.Minute

// ReconciliationLoop runs Reconciliation on a schedule and turns its report
// into metrics.
//
// B4.5 delivered the algorithm and B5 delivered an on-demand endpoint, but
// nothing ran it periodically, so "the books do not balance" was a question
// someone had to think to ask. E1 recorded this as the reason no alert could
// exist for it. This loop is the missing consumer.
type ReconciliationLoop struct {
	Reconciliation Reconciliation
	// Metrics is optional: a nil registry disables the signals, not the run.
	Metrics *observability.Registry

	// Window and Lag default to the constants above when zero.
	Window time.Duration
	Lag    time.Duration
}

// ReconciliationRunResult is what one scheduled run observed. It is returned
// for tests and logging; the durable output is the metrics.
type ReconciliationRunResult struct {
	Start         time.Time
	End           time.Time
	Balanced      bool
	Discrepancies map[string]int
	UnsettledRows int
	TenantCount   int
}

// RunOnce reconciles [now-lag-window, now-lag) and publishes the result.
//
// It never writes -- Reconciliation.Run runs in a read-only transaction, and
// this loop only adds metrics on top. A scheduled job that could repair what
// it audits would stop being an independent witness (see billing_reconcile.go).
func (l ReconciliationLoop) RunOnce(ctx context.Context, now time.Time) (ReconciliationRunResult, error) {
	window, lag := l.Window, l.Lag
	if window <= 0 {
		window = reconcileWindow
	}
	if lag <= 0 {
		lag = reconcileLag
	}
	if lag <= billingRunInterval {
		// Refuse rather than emit a permanently-unbalanced signal. A wrong lag
		// does not corrupt anything, but it does destroy the metric's meaning,
		// and a silently meaningless alert is the failure E1 set out to avoid.
		return ReconciliationRunResult{}, fmt.Errorf(
			"reconciliation lag %s must exceed the billing interval %s, or every run reports rows the deduction job has not reached yet",
			lag, billingRunInterval)
	}
	end := now.Add(-lag)
	start := end.Add(-window)
	report, err := l.Reconciliation.Run(ctx, start, end)
	if err != nil {
		l.runCounter("error")
		return ReconciliationRunResult{Start: start, End: end}, err
	}
	result := ReconciliationRunResult{
		Start:         start,
		End:           end,
		Balanced:      report.Balanced(),
		Discrepancies: map[string]int{},
		UnsettledRows: len(report.UnsettledRows),
		TenantCount:   len(report.Tenants),
	}
	for _, discrepancy := range report.Discrepancies {
		result.Discrepancies[discrepancy.Kind]++
	}
	l.publish(result)
	return result, nil
}

// publish reports every discrepancy kind on every run, including the kinds
// that found nothing.
//
// Emitting only the non-zero kinds would make a gauge that never resets: a
// kind that appeared once and then cleared would keep its last value forever,
// so the alert would stay firing after the problem was fixed. Every known kind
// is therefore written each run, zero included.
func (l ReconciliationLoop) publish(result ReconciliationRunResult) {
	if l.Metrics == nil {
		return
	}
	for _, kind := range reconciliationKinds {
		l.Metrics.SetGauge("control_billing_reconcile_discrepancies", float64(result.Discrepancies[kind]), "kind", kind)
	}
	l.Metrics.SetGauge("control_billing_reconcile_unsettled_rows", float64(result.UnsettledRows))
	l.runCounter("ok")
	balanced := 0.0
	if result.Balanced {
		balanced = 1
	}
	l.Metrics.SetGauge("control_billing_reconcile_balanced", balanced)
}

// runCounter records that a run happened at all. Without it, a loop that stopped
// ticking would look identical to a loop that keeps finding nothing wrong --
// both leave the discrepancy gauges at zero.
func (l ReconciliationLoop) runCounter(result string) {
	if l.Metrics == nil {
		return
	}
	l.Metrics.AddCounter("control_billing_reconcile_runs_total", 1, "result", result)
	if result == "ok" {
		l.Metrics.SetGauge("control_billing_reconcile_last_success_seconds", float64(time.Now().Unix()))
	}
}

// reconciliationKinds is every discrepancy kind billing_reconcile.go can
// produce. It is a package-level list so publish cannot drift from the
// constants; TestReconciliationKindsCoversEveryDiscrepancyConstant guards it.
var reconciliationKinds = []string{
	DiscrepancyBilledWithoutAmount,
	DiscrepancyBilledWithoutRun,
	DiscrepancyAmountOnUnsettledRow,
	DiscrepancyRunDebitMismatch,
	DiscrepancyWalletBalanceDrift,
	DiscrepancyMissingWallet,
}
