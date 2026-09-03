package contracts

import (
	"testing"
	"time"
)

func TestTerminalEventIDIsDeterministic(t *testing.T) {
	if got, want := TerminalEventID("attempt-1"), TerminalEventID("attempt-1"); got != want {
		t.Fatalf("terminal ID changed: %q != %q", got, want)
	}
	if TerminalEventID("attempt-1") == TerminalEventID("attempt-2") {
		t.Fatal("different attempts must have different terminal IDs")
	}
}

func TestStreamEventRejectsUnknownMajor(t *testing.T) {
	event, err := NewStreamEvent(EventTypeRelease, "event", "request", "attempt", "producer", timeNow(), map[string]string{"ok": "1"})
	if err != nil {
		t.Fatal(err)
	}
	event.SchemaVersion = 101
	if err := event.Validate(); err == nil {
		t.Fatal("unknown major schema must be rejected")
	}
}

func TestAttemptStartedValidatesRequiredFields(t *testing.T) {
	now := timeNow()
	attempt := AttemptStarted{SchemaVersion: SchemaVersion, EventID: "event", RequestID: "request", AttemptID: "attempt", AttemptNo: 1, ProducerID: "producer", OccurredAt: now, AccountID: "account", Provider: "apikey", Model: "model", TenantID: "tenant", DeadlineAt: now.Add(time.Minute)}
	if err := attempt.Validate(); err != nil {
		t.Fatal(err)
	}
	attempt.TenantID = ""
	if err := attempt.Validate(); err == nil {
		t.Fatal("missing tenant must be rejected")
	}
}

func timeNow() (now time.Time) { return time.Now() }
