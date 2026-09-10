package detail

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elucid/gateway-platform/internal/observability"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

// recordingWriter captures batches and can be told to fail.
type recordingWriter struct {
	mu      sync.Mutex
	batches [][]Record
	err     error
	// block, when set, holds WriteBatch until it is closed. Used to prove a
	// stalled ClickHouse cannot stall the caller.
	block chan struct{}
}

func (w *recordingWriter) WriteBatch(_ context.Context, records []Record) error {
	if w.block != nil {
		<-w.block
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	batch := append([]Record(nil), records...)
	w.batches = append(w.batches, batch)
	return w.err
}

func (w *recordingWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	total := 0
	for _, batch := range w.batches {
		total += len(batch)
	}
	return total
}

func testRelease(eventID string) contracts.Release {
	attemptID := "attempt-" + eventID
	return contracts.Release{
		SchemaVersion: contracts.SchemaVersion, EventID: contracts.TerminalEventID(attemptID),
		RequestID: "req-" + eventID, AttemptID: attemptID, AttemptNo: 2,
		ProducerID: "gw-node-1", OccurredAt: time.Date(2026, 9, 11, 8, 30, 0, 0, time.UTC),
		AccountID: "acct-1", Provider: "anthropic", Model: "claude-opus-5", TenantID: "tenant-1",
		StatusCode: 200, LatencyMS: 1234, ErrorClass: contracts.ErrorOK,
		UsageSource: contracts.UsageSourceUpstream, Partial: true,
		TokensIn: 11, TokensOut: 22, CacheReadTokens: 33, CacheWriteTokens: 44,
		InboundProtocol: "openai_chat",
	}
}

func counterValue(t *testing.T, registry *observability.Registry, prefix string) float64 {
	t.Helper()
	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			t.Fatalf("metric %q: %v", line, err)
		}
		return value
	}
	return 0
}

// The A12 projection: every Release field that is not a secret reaches the
// record, and the retry chain (attempt_no) and routing (producer/tenant/
// account/provider/protocol) survive.
func TestFromReleaseCarriesRoutingAndOutcome(t *testing.T) {
	release := testRelease("a")
	ingested := time.Date(2026, 9, 11, 8, 30, 5, 0, time.UTC)
	record := FromRelease(release, ingested)

	if record.EventID != release.EventID || record.RequestID != release.RequestID || record.AttemptID != release.AttemptID {
		t.Errorf("identity lost: %+v", record)
	}
	if record.AttemptNo != 2 {
		t.Errorf("attempt_no = %d, want 2; without it the retry chain is unreadable", record.AttemptNo)
	}
	if record.ProducerID != "gw-node-1" || record.TenantID != "tenant-1" || record.AccountID != "acct-1" ||
		record.Provider != "anthropic" || record.InboundProtocol != "openai_chat" {
		t.Errorf("routing lost: %+v", record)
	}
	if record.OccurredAt != "2026-09-11 08:30:00.000" || record.IngestedAt != "2026-09-11 08:30:05.000" {
		t.Errorf("timestamps = %q / %q", record.OccurredAt, record.IngestedAt)
	}
	// occurred_at and ingested_at are separate on purpose: the gap between them
	// is the detail pipeline's lag, and collapsing them would hide it.
	if record.OccurredAt == record.IngestedAt {
		t.Error("occurred_at and ingested_at must stay distinct")
	}
	if record.Partial != 1 {
		t.Errorf("partial = %d, want 1", record.Partial)
	}
	if record.TokensIn != 11 || record.TokensOut != 22 || record.CacheReadTokens != 33 || record.CacheWriteTokens != 44 {
		t.Errorf("token counts lost: %+v", record)
	}
}

// A12's DoD: credentials, plaintext tokens and refresh tokens must never enter
// detail. That is guaranteed by Record having nowhere for them to go -- no map,
// no interface, no byte slice, no body field. This test is the tripwire on that
// guarantee; if it fails, someone added a field that can carry a secret.
func TestRecordCarriesNoFreeFormFields(t *testing.T) {
	recordType := reflect.TypeOf(Record{})
	for i := 0; i < recordType.NumField(); i++ {
		field := recordType.Field(i)
		switch field.Type.Kind() {
		case reflect.String, reflect.Int, reflect.Int32, reflect.Int64, reflect.Uint8:
		default:
			t.Errorf("field %s is a %s: detail may only hold scalars, or a credential can be smuggled in",
				field.Name, field.Type.Kind())
		}
		name := strings.ToLower(field.Name)
		for _, forbidden := range []string{"token", "secret", "credential", "auth", "header", "body", "prompt", "response", "cookie"} {
			// TokensIn / TokensOut are counts, not tokens; everything else
			// matching these words has no business in detail.
			if strings.Contains(name, forbidden) && !strings.HasPrefix(name, "tokens") && !strings.Contains(name, "cachewrite") && !strings.Contains(name, "cacheread") {
				t.Errorf("field %s looks like it carries %s material", field.Name, forbidden)
			}
		}
	}
}

// The core A12 guarantee, stated as a test: a sink whose store is dead loses
// detail and nothing else. Observe returns no error, so there is no value a
// caller could propagate into the ledger transaction or use to withhold an ACK.
func TestObserveNeverFailsWhenTheStoreIsDown(t *testing.T) {
	registry := observability.NewRegistry()
	writer := &recordingWriter{err: errors.New("clickhouse is down")}
	sink := &Sink{Writer: writer, Metrics: registry, BatchSize: 1, FlushInterval: 10 * time.Millisecond}
	sink.Start()
	defer sink.Stop()

	// Observe has no error return at all -- this call compiling is half the
	// proof, and the other half is that it returns promptly.
	done := make(chan struct{})
	go func() {
		sink.Observe(testRelease("down"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Observe blocked while the store was failing")
	}

	if !sink.WaitForFlush(2 * time.Second) {
		t.Fatal("the batcher never attempted a flush")
	}
	if got := counterValue(t, registry, "detail_flush_failures_total"); got < 1 {
		t.Errorf("a failed flush was not counted (%v): silent detail loss is indistinguishable from silent traffic loss", got)
	}
	if got := counterValue(t, registry, `detail_dropped_total{reason="write_failed"}`); got < 1 {
		t.Errorf("dropped records were not counted: %v", got)
	}
}

// A stalled store must not become back-pressure on the metering path. The
// bound is memory, and past it detail is dropped rather than queued.
func TestObserveDropsRatherThanBlockingWhenTheBufferIsFull(t *testing.T) {
	registry := observability.NewRegistry()
	writer := &recordingWriter{block: make(chan struct{})}
	sink := &Sink{Writer: writer, Metrics: registry, BatchSize: 1, FlushInterval: time.Millisecond, BufferSize: 4}
	sink.Start()
	defer func() {
		close(writer.block)
		sink.Stop()
	}()

	// Far more records than the buffer holds, into a sink whose writer is
	// wedged. If Observe blocked, this loop would never finish.
	start := time.Now()
	for i := 0; i < 500; i++ {
		sink.Observe(testRelease("full-" + strconv.Itoa(i)))
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("500 Observe calls took %s against a wedged store; the metering path is being throttled by detail", elapsed)
	}
	if got := counterValue(t, registry, `detail_dropped_total{reason="buffer_full"}`); got < 1 {
		t.Errorf("no buffer_full drops were counted, so the bound is not doing anything: %v", got)
	}
}

// The happy path: records reach the writer, batched.
func TestSinkFlushesObservedRecords(t *testing.T) {
	writer := &recordingWriter{}
	sink := &Sink{Writer: writer, Metrics: observability.NewRegistry(), BatchSize: 3, FlushInterval: 10 * time.Millisecond}
	sink.Start()
	for i := 0; i < 3; i++ {
		sink.Observe(testRelease("ok-" + strconv.Itoa(i)))
	}
	sink.Stop()
	if got := writer.count(); got != 3 {
		t.Fatalf("writer received %d records, want 3", got)
	}
}

// A nil sink is a valid "detail disabled" deployment, and must not panic on
// the metering path.
func TestNilSinkIsADisabledSink(t *testing.T) {
	var sink *Sink
	sink.Observe(testRelease("nil"))
	sink.Stop()
	// An unstarted sink is the other half of the same case.
	(&Sink{}).Observe(testRelease("unstarted"))
}
