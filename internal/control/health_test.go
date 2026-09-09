package control

import (
	"testing"
	"time"

	"github.com/elucid/gateway-platform/internal/observability"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

func TestPlatformSignalsAlertOnlyWhenSeveralAccountsFailTogether(t *testing.T) {
	now := time.Unix(10_000, 0)
	metrics := observability.NewRegistry()
	signals := NewPlatformSignals(metrics)
	signals.Now = func() time.Time { return now }
	signals.Window = 30 * time.Second
	signals.Threshold = 2

	release := func(provider, account string, class contracts.ErrorClass) contracts.Release {
		return contracts.Release{Provider: provider, AccountID: account, ErrorClass: class}
	}
	// one account failing repeatedly is account noise, not a platform signal
	for i := 0; i < 5; i++ {
		if signals.Observe(release("codex", "a", contracts.ErrorForbiddenTransport)) {
			t.Fatal("single account raised a platform alert")
		}
	}
	if got := signals.Alerting(); len(got) != 0 {
		t.Fatalf("alerting=%v", got)
	}
	// non-transport classes never count
	if signals.Observe(release("codex", "b", contracts.ErrorRateLimitedUnknown)) || signals.Observe(release("codex", "c", contracts.ErrorAuthInvalid)) {
		t.Fatal("non-transport class counted toward the platform signal")
	}
	// 5xx is recorded for the breaker counter but is not a fingerprint signal
	if signals.Observe(release("codex", "b", contracts.ErrorUpstream5xx)) {
		t.Fatal("5xx raised a transport alert")
	}
	// a second distinct account inside the window crosses the threshold
	if !signals.Observe(release("codex", "b", contracts.ErrorBlocked)) {
		t.Fatal("two accounts inside the window did not alert")
	}
	if got := signals.Alerting(); len(got) != 1 || got[0] != "codex" {
		t.Fatalf("alerting=%v", got)
	}
	// other platforms are independent
	if signals.Observe(release("claude", "x", contracts.ErrorBlocked)) {
		t.Fatal("claude alerted from codex events")
	}
	// window expiry clears the alert
	now = now.Add(31 * time.Second)
	if got := signals.Alerting(); len(got) != 0 {
		t.Fatalf("alert survived the window: %v", got)
	}
	if signals.Observe(release("codex", "c", contracts.ErrorForbiddenTransport)) {
		t.Fatal("stale events still counted after the window")
	}
	// nil receiver is safe (Ledger without Signals)
	var none *PlatformSignals
	if none.Observe(release("codex", "z", contracts.ErrorBlocked)) || none.Alerting() != nil {
		t.Fatal("nil PlatformSignals must be inert")
	}
}
