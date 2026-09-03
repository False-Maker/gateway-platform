package control

import (
	"sync"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

type AttemptState string

const (
	AttemptOpen       AttemptState = "open"
	AttemptTerminal   AttemptState = "terminal"
	AttemptReconciled AttemptState = "reconciled"
)

type AttemptRecord struct {
	Started contracts.AttemptStarted
	State   AttemptState
	Release *contracts.Release
}

// MemoryLedger is a deterministic model used by focused tests and local
// dry-runs. Ledger's PG implementation in ledger.go applies the same rules.
type MemoryLedger struct {
	mu       sync.Mutex
	attempts map[string]*AttemptRecord
	events   map[string]struct{}
}

func NewMemoryLedger() *MemoryLedger {
	return &MemoryLedger{attempts: make(map[string]*AttemptRecord), events: make(map[string]struct{})}
}

func (m *MemoryLedger) ApplyAttemptStarted(event contracts.AttemptStarted) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.attempts[event.AttemptID]; exists {
		return false
	}
	m.attempts[event.AttemptID] = &AttemptRecord{Started: event, State: AttemptOpen}
	return true
}

func (m *MemoryLedger) ApplyRelease(release contracts.Release) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.events[release.EventID]; exists {
		return false
	}
	if record, exists := m.attempts[release.AttemptID]; exists {
		if record.Release != nil {
			return false
		}
		record.Release = &release
		record.State = AttemptTerminal
	}
	m.events[release.EventID] = struct{}{}
	return true
}

func (m *MemoryLedger) RecoverExpired(now time.Time) []contracts.Release {
	m.mu.Lock()
	defer m.mu.Unlock()
	var recovered []contracts.Release
	for _, record := range m.attempts {
		if record.State != AttemptOpen || !record.Started.DeadlineAt.Before(now) {
			continue
		}
		eventID := contracts.TerminalEventID(record.Started.AttemptID)
		if _, exists := m.events[eventID]; exists {
			record.State = AttemptReconciled
			continue
		}
		release := contracts.Release{SchemaVersion: contracts.SchemaVersion, EventID: eventID, RequestID: record.Started.RequestID, AttemptID: record.Started.AttemptID, AttemptNo: record.Started.AttemptNo, ProducerID: "control-reconciler", OccurredAt: now, AccountID: record.Started.AccountID, Provider: record.Started.Provider, StatusCode: 504, Model: record.Started.Model, TenantID: record.Started.TenantID, ErrorClass: contracts.ErrorNetwork, UsageSource: contracts.UsageSourceMissing, Partial: true}
		record.Release = &release
		record.State = AttemptReconciled
		m.events[eventID] = struct{}{}
		recovered = append(recovered, release)
	}
	return recovered
}
