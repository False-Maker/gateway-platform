package control

import (
	"testing"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

func testStarted(deadline time.Time) contracts.AttemptStarted {
	return contracts.AttemptStarted{SchemaVersion: contracts.SchemaVersion, EventID: "start-1", RequestID: "request-1", AttemptID: "attempt-1", AttemptNo: 1, ProducerID: "gateway-1", OccurredAt: deadline.Add(-time.Minute), DeadlineAt: deadline, AccountID: "account-1", Provider: "apikey", Model: "m", TenantID: "tenant-1"}
}

func TestMemoryLedgerReleaseAndRecoveryAreIdempotent(t *testing.T) {
	now := time.Unix(1000, 0)
	ledger := NewMemoryLedger()
	started := testStarted(now.Add(-time.Second))
	if !ledger.ApplyAttemptStarted(started) || ledger.ApplyAttemptStarted(started) {
		t.Fatal("attempt_started must be idempotent")
	}
	releases := ledger.RecoverExpired(now)
	if len(releases) != 1 || releases[0].UsageSource != contracts.UsageSourceMissing || !releases[0].Partial {
		t.Fatalf("unexpected recovery: %#v", releases)
	}
	if got := ledger.RecoverExpired(now.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("recovery duplicated: %#v", got)
	}
	if ledger.ApplyRelease(releases[0]) {
		t.Fatal("synthetic release must already be idempotent")
	}
}
