package detail

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/elucid/gateway-platform/internal/observability"
)

// openClickHouse returns a writer against the test container, or skips.
// Detail is droppable by design, so a missing store must not fail the suite --
// it must skip, exactly as a deployment without ClickHouse simply has no detail.
func openClickHouse(t *testing.T) (context.Context, ClickHouseWriter) {
	t.Helper()
	endpoint := os.Getenv("GATEWAY_TEST_CLICKHOUSE_URL")
	if endpoint == "" {
		t.Skip("GATEWAY_TEST_CLICKHOUSE_URL is not set")
	}
	ctx := context.Background()
	writer := ClickHouseWriter{URL: endpoint, Database: "gateway_detail_test", Table: "request_detail"}
	if err := writer.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	return ctx, writer
}

// query runs one SQL statement and returns the trimmed body.
func (w ClickHouseWriter) query(ctx context.Context, t *testing.T, sql string) string {
	t.Helper()
	var out strings.Builder
	if err := w.execInto(ctx, sql, nil, &out); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out.String())
}

// The DDL applies against a real ClickHouse, records survive a round trip with
// every field intact, and the schema is idempotent the way migrations.Apply is.
func TestClickHouseWriterRoundTripsARecord(t *testing.T) {
	ctx, writer := openClickHouse(t)
	const tenantID = "a12-roundtrip-tenant"
	cleanup := func() {
		_ = writer.exec(context.Background(),
			"ALTER TABLE gateway_detail_test.request_detail DELETE WHERE tenant_id='"+tenantID+"'", nil)
	}
	cleanup()
	t.Cleanup(cleanup)

	// Idempotent: EnsureSchema already ran once in openClickHouse.
	if err := writer.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema is not idempotent: %v", err)
	}

	release := testRelease("roundtrip")
	release.TenantID = tenantID
	record := FromRelease(release, time.Date(2026, 9, 11, 8, 30, 5, 0, time.UTC))
	if err := writer.WriteBatch(ctx, []Record{record}); err != nil {
		t.Fatal(err)
	}

	got := writer.query(ctx, t, `SELECT attempt_no, producer_id, account_id, provider, model,
		inbound_protocol, status_code, latency_ms, error_class, usage_source, partial,
		tokens_in, tokens_out, cache_read_tokens, cache_write_tokens,
		toString(occurred_at), toString(ingested_at)
		FROM gateway_detail_test.request_detail WHERE tenant_id='`+tenantID+`' FORMAT TSV`)
	want := strings.Join([]string{
		"2", "gw-node-1", "acct-1", "anthropic", "claude-opus-5",
		"openai_chat", "200", "1234", "ok", "upstream", "1",
		"11", "22", "33", "44",
		"2026-09-11 08:30:00.000", "2026-09-11 08:30:05.000",
	}, "\t")
	if got != want {
		t.Errorf("round trip changed the record:\n got %q\nwant %q", got, want)
	}
}

// The store is fed by an at-least-once pipeline: the sink writes after the
// ledger transaction commits, so a consumer restart replays the release. The
// engine has to converge on one row rather than double-count.
func TestRedeliveredReleaseDoesNotDuplicate(t *testing.T) {
	ctx, writer := openClickHouse(t)
	const tenantID = "a12-redelivery-tenant"
	cleanup := func() {
		_ = writer.exec(context.Background(),
			"ALTER TABLE gateway_detail_test.request_detail DELETE WHERE tenant_id='"+tenantID+"'", nil)
	}
	cleanup()
	t.Cleanup(cleanup)

	release := testRelease("redelivery")
	release.TenantID = tenantID
	first := FromRelease(release, time.Date(2026, 9, 11, 8, 30, 5, 0, time.UTC))
	second := FromRelease(release, time.Date(2026, 9, 11, 8, 31, 0, 0, time.UTC))
	for _, record := range []Record{first, second} {
		if err := writer.WriteBatch(ctx, []Record{record}); err != nil {
			t.Fatal(err)
		}
	}

	// FINAL forces the ReplacingMergeTree collapse that a background merge
	// would eventually do on its own. The count without FINAL is deliberately
	// not asserted: it is 2 until the merge runs, which is exactly why detail
	// must never be summed as if it were the ledger.
	got := writer.query(ctx, t,
		"SELECT count(), max(ingested_at) FROM gateway_detail_test.request_detail FINAL WHERE tenant_id='"+tenantID+"' FORMAT TSV")
	want := "1\t2026-09-11 08:31:00.000"
	if got != want {
		t.Errorf("redelivery collapsed to %q, want %q", got, want)
	}
}

// End to end through the async sink: records observed on the caller's side turn
// up in ClickHouse without the caller ever waiting for them.
func TestSinkDeliversToClickHouse(t *testing.T) {
	ctx, writer := openClickHouse(t)
	const tenantID = "a12-sink-tenant"
	cleanup := func() {
		_ = writer.exec(context.Background(),
			"ALTER TABLE gateway_detail_test.request_detail DELETE WHERE tenant_id='"+tenantID+"'", nil)
	}
	cleanup()
	t.Cleanup(cleanup)

	sink := &Sink{Writer: writer, Metrics: observability.NewRegistry(), BatchSize: 10, FlushInterval: 50 * time.Millisecond}
	sink.Start()
	const count = 10
	for i := 0; i < count; i++ {
		release := testRelease("sink-" + strconv.Itoa(i))
		release.TenantID = tenantID
		sink.Observe(release)
	}
	sink.Stop()

	got := writer.query(ctx, t,
		"SELECT count() FROM gateway_detail_test.request_detail WHERE tenant_id='"+tenantID+"' FORMAT TSV")
	if got != strconv.Itoa(count) {
		t.Errorf("ClickHouse holds %s of %d observed records", got, count)
	}
}

// A ClickHouse that rejects the write must surface as a counted drop, not as an
// error the caller could ever see. This exercises the real HTTP error path
// rather than a stub, so a change in ClickHouse's error handling is caught.
func TestRealClickHouseErrorsStayInsideTheSink(t *testing.T) {
	ctx, writer := openClickHouse(t)
	broken := writer
	broken.Table = "table_that_does_not_exist"
	if err := broken.WriteBatch(ctx, []Record{FromRelease(testRelease("broken"), time.Now())}); err == nil {
		t.Fatal("writing to a missing table succeeded; the error path below proves nothing")
	}

	registry := observability.NewRegistry()
	sink := &Sink{Writer: broken, Metrics: registry, BatchSize: 1, FlushInterval: 10 * time.Millisecond}
	sink.Start()
	sink.Observe(testRelease("broken-sink"))
	if !sink.WaitForFlush(5 * time.Second) {
		t.Fatal("no flush was attempted")
	}
	sink.Stop()
	if got := counterValue(t, registry, "detail_flush_failures_total"); got < 1 {
		t.Errorf("a real ClickHouse rejection was not counted: %v", got)
	}
}
