package events

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elucid/gateway-platform/internal/observability"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

type testHandler struct {
	attempts, releases int
	failOnce           bool
	failAlways         bool
	committed          bool
	failAfterCommit    bool
}

func (h *testHandler) HandleAttemptStarted(context.Context, contracts.AttemptStarted) error {
	h.attempts++
	if h.failAfterCommit && !h.committed {
		h.committed = true
		return errors.New("crash after commit before ack")
	}
	if h.failAlways {
		return errors.New("persistent")
	}
	if h.failOnce {
		h.failOnce = false
		return errors.New("transient")
	}
	return nil
}
func (h *testHandler) HandleRelease(context.Context, contracts.Release) error {
	h.releases++
	return nil
}
func (h *testHandler) RecoverAttempts(context.Context, time.Time) (int, error) { return 0, nil }

func validEvent(t *testing.T) contracts.StreamEvent {
	t.Helper()
	now := time.Now().UTC()
	e, err := contracts.NewStreamEvent(contracts.EventTypeAttemptStarted, "e1", "r1", "a1", "p1", now, contracts.AttemptStarted{SchemaVersion: 1, EventID: "e1", RequestID: "r1", AttemptID: "a1", AttemptNo: 1, ProducerID: "p1", OccurredAt: now, DeadlineAt: now.Add(time.Minute), AccountID: "acct", Provider: "apikey", Model: "m", TenantID: "tenant"})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestCommittedMessageIsReclaimedAfterCrashBeforeAck(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	handler := &testHandler{failAfterCommit: true}
	consumer := Consumer{Redis: rdb, Consumer: "c1", Handler: handler, Now: time.Now}
	if _, err := (Producer{Redis: rdb, ProducerID: "p1"}).Add(context.Background(), validEvent(t)); err != nil {
		t.Fatal(err)
	}
	if err := consumer.RunOnce(context.Background()); err == nil {
		t.Fatal("simulated post-commit crash must leave the message pending")
	}
	if got := rdb.XPending(context.Background(), StreamKey, GroupName).Val().Count; got != 1 {
		t.Fatalf("expected one pending message, got %d", got)
	}
	mini.SetTime(time.Now().Add(ReclaimIdle + time.Second))
	if err := consumer.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !handler.committed || handler.attempts != 2 {
		t.Fatalf("committed message was not redelivered: %#v", handler)
	}
	if got := rdb.XPending(context.Background(), StreamKey, GroupName).Val().Count; got != 0 {
		t.Fatalf("redelivered message was not acked: %d", got)
	}
}

func TestInvalidPayloadGoesDirectlyToDLQ(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	handler := &testHandler{}
	consumer := Consumer{Redis: rdb, Consumer: "c1", Handler: handler, Now: time.Now}
	event := validEvent(t)
	event.Payload = []byte(`{}`)
	if _, err := (Producer{Redis: rdb, ProducerID: "p1"}).Add(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if err := consumer.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if handler.attempts != 0 || rdb.XLen(context.Background(), DLQKey).Val() != 1 {
		t.Fatalf("invalid payload was handled or not dead-lettered: attempts=%d", handler.attempts)
	}
	if keys := mini.Keys(); containsKey(keys, "gateway:events:retry:") {
		t.Fatalf("permanent contract failure created retry state: %v", keys)
	}
}

func containsKey(keys []string, prefix string) bool {
	for _, key := range keys {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

func TestPendingReclaimAndDLQ(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	handler := &testHandler{failOnce: true}
	consumer := Consumer{Redis: rdb, Consumer: "c1", Handler: handler, Now: time.Now}
	if _, err := (Producer{Redis: rdb, ProducerID: "p1"}).Add(context.Background(), validEvent(t)); err != nil {
		t.Fatal(err)
	}
	if err := consumer.RunOnce(context.Background()); err == nil {
		t.Fatal("first transient handler failure should leave pending")
	}
	mini.SetTime(time.Now().Add(ReclaimIdle + time.Second))
	if err := consumer.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if handler.attempts != 2 {
		t.Fatalf("expected reclaim delivery, got %d attempts", handler.attempts)
	}
	if got := rdb.XPending(context.Background(), StreamKey, GroupName).Val().Count; got != 0 {
		t.Fatalf("pending remains: %d", got)
	}
	if _, err := rdb.XAdd(context.Background(), &redis.XAddArgs{Stream: StreamKey, Values: map[string]any{"event_type": "release"}}).Result(); err != nil {
		t.Fatal(err)
	}
	if err := consumer.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := rdb.XLen(context.Background(), DLQKey).Val(); got != 1 {
		t.Fatalf("expected one DLQ record, got %d", got)
	}
}

func TestRetryLimitMovesMessageToDLQ(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	handler := &testHandler{failAlways: true}
	consumer := Consumer{Redis: rdb, Consumer: "c1", Handler: handler, Now: time.Now}
	baseTime := time.Now()
	if _, err := (Producer{Redis: rdb, ProducerID: "p1"}).Add(context.Background(), validEvent(t)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxRetries; i++ {
		err := consumer.RunOnce(context.Background())
		if err == nil {
			t.Fatal("persistent handler failure must remain pending")
		}
		mini.SetTime(baseTime.Add(time.Duration(i+1) * (ReclaimIdle + time.Second)))
	}
	if err := consumer.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := rdb.XLen(context.Background(), DLQKey).Val(); got != 1 {
		t.Fatalf("expected retry-limit DLQ, got %d", got)
	}
}

func TestTrimPreservesOldestPendingMessage(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-Retention - time.Hour).UnixMilli()
	recent := now.Add(-time.Hour).UnixMilli()
	ids := []string{fmt.Sprintf("%d-0", old), fmt.Sprintf("%d-1", old), fmt.Sprintf("%d-0", recent)}
	for _, id := range ids {
		if _, err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: StreamKey, ID: id, Values: map[string]any{"value": id}}).Result(); err != nil {
			t.Fatal(err)
		}
	}
	if err := rdb.XGroupCreate(ctx, StreamKey, GroupName, "0").Err(); err != nil {
		t.Fatal(err)
	}
	streams, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{Group: GroupName, Consumer: "c1", Streams: []string{StreamKey, ">"}, Count: 2}).Result()
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.XAck(ctx, StreamKey, GroupName, streams[0].Messages[0].ID).Err(); err != nil {
		t.Fatal(err)
	}
	consumer := Consumer{Redis: rdb, Now: func() time.Time { return now }}
	if err := consumer.Trim(ctx); err != nil {
		t.Fatal(err)
	}
	entries, err := rdb.XRange(ctx, StreamKey, "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].ID != ids[1] || entries[1].ID != ids[2] {
		t.Fatalf("trim crossed pending boundary: %#v", entries)
	}
}

// A22 pins the enumeration PrimeMetrics depends on. A hand-kept list of DLQ
// categories would rot the first time someone adds a rejection path, and the
// symptom would be invisible: one unprimed series, one alert that misses the
// first message of that category. So read the call sites instead of trusting
// the list -- the same approach alertrules_test.go uses for metric names.
func TestDLQCategoriesCoverEveryDeadLetterCall(t *testing.T) {
	source, err := os.ReadFile("stream.go")
	if err != nil {
		t.Fatal(err)
	}
	calls := regexp.MustCompile(`deadLetter\(ctx, message, "([a-z_]+)"`).FindAllSubmatch(source, -1)
	if len(calls) == 0 {
		t.Fatal("found no deadLetter call sites; the scanner is broken")
	}
	declared := make(map[string]bool)
	for _, category := range dlqCategories() {
		declared[category] = true
	}
	used := make(map[string]bool)
	for _, call := range calls {
		category := string(call[1])
		used[category] = true
		if !declared[category] {
			t.Errorf("deadLetter is called with %q but dlqCategories() omits it: that series is never primed", category)
		}
	}
	for category := range declared {
		if !used[category] {
			t.Errorf("dlqCategories() lists %q but no deadLetter call produces it", category)
		}
	}
}

func TestPrimeMetricsCreatesZeroSeriesForEveryDLQCategory(t *testing.T) {
	metrics := observability.NewRegistry()
	consumer := Consumer{Metrics: metrics}

	// Guard against an empty assertion: if the series already existed the
	// checks below would pass without proving priming does anything.
	if before := scrapeRegistry(t, metrics); before != "" {
		t.Fatalf("registry was not empty before priming: %s", before)
	}

	consumer.PrimeMetrics()
	primed := scrapeRegistry(t, metrics)
	for _, category := range dlqCategories() {
		want := `control_stream_dlq_total{reason="` + category + `"} 0`
		if !strings.Contains(primed, want) {
			t.Errorf("missing primed series %s; got:\n%s", want, primed)
		}
	}
	if want := `request_attempt_recovered_total{source="synthetic"} 0`; !strings.Contains(primed, want) {
		t.Errorf("missing primed series %s; got:\n%s", want, primed)
	}

	// The transition a zero start buys: 0 -> 1 is what increase() can see.
	consumer.metrics().AddCounter("control_stream_dlq_total", 1, "reason", "invalid_json")
	after := scrapeRegistry(t, metrics)
	if !strings.Contains(after, `control_stream_dlq_total{reason="invalid_json"} 1`) {
		t.Errorf("the primed series did not advance; got:\n%s", after)
	}
	if !strings.Contains(after, `control_stream_dlq_total{reason="retry_exhausted"} 0`) {
		t.Errorf("an unrelated category moved; got:\n%s", after)
	}
}

func scrapeRegistry(t *testing.T, registry *observability.Registry) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	return recorder.Body.String()
}
