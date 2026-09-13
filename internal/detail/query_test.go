package detail

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// The read path's column list is derived from Record's json tags, so a field
// added to Record is queryable without a second edit. This asserts the
// derivation rather than a hand-written list, because a hand-written list is
// exactly what would drift.
func TestRecordColumnsMatchTheStruct(t *testing.T) {
	recordType := reflect.TypeOf(Record{})
	if len(recordColumns) != recordType.NumField() {
		t.Fatalf("column list has %d entries for %d struct fields", len(recordColumns), recordType.NumField())
	}
	for i := 0; i < recordType.NumField(); i++ {
		tag, _, _ := strings.Cut(recordType.Field(i).Tag.Get("json"), ",")
		if recordColumns[i] != tag {
			t.Errorf("column %d is %q but field %s is tagged %q", i, recordColumns[i], recordType.Field(i).Name, tag)
		}
	}
}

// A query against an unconfigured store is a named error, not an empty result:
// "detail is off" and "nothing matched" must never look the same to a caller.
func TestQueryWithoutAStoreIsANamedError(t *testing.T) {
	_, err := ClickHouseWriter{}.Query(context.Background(), QueryFilter{})
	if err != ErrStoreDisabled {
		t.Fatalf("got %v, want ErrStoreDisabled", err)
	}
}

func TestParseLimitRejectsNonPositiveValues(t *testing.T) {
	for _, raw := range []string{"0", "-1", "many", "1.5"} {
		if _, err := ParseLimit(raw); err == nil {
			t.Errorf("ParseLimit(%q) was accepted", raw)
		}
	}
	// Absent and whitespace-only both mean "unset"; the caller then applies
	// its own default rather than being handed an error for a missing filter.
	for _, raw := range []string{"", " "} {
		limit, err := ParseLimit(raw)
		if err != nil || limit != 0 {
			t.Errorf("ParseLimit(%q) = %d, %v; want 0, nil", raw, limit, err)
		}
	}
	if limit, err := ParseLimit("50"); err != nil || limit != 50 {
		t.Errorf("ParseLimit(\"50\") = %d, %v", limit, err)
	}
}

// JSONEachRow output decodes back into the same record that was written, with
// the timestamps in the form FromRelease produces. A round trip that changed
// the timestamp format would make the console's output disagree with the
// insert path for no visible reason.
func TestDecodeRecordsReadsJSONEachRow(t *testing.T) {
	body := strings.NewReader(
		`{"event_id":"e1","request_id":"r1","attempt_id":"a1","attempt_no":2,` +
			`"occurred_at":"2026-09-11 08:30:00.000","ingested_at":"2026-09-11 08:30:05.000",` +
			`"producer_id":"gw-1","tenant_id":"t1","account_id":"acc1","provider":"anthropic",` +
			`"model":"claude-opus-5","inbound_protocol":"openai_chat","status_code":200,` +
			`"latency_ms":1234,"error_class":"ok","usage_source":"upstream","partial":1,` +
			`"tokens_in":11,"tokens_out":22,"cache_read_tokens":33,"cache_write_tokens":44}` + "\n")
	records, err := decodeRecords(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("decoded %d records, want 1", len(records))
	}
	want := Record{
		EventID: "e1", RequestID: "r1", AttemptID: "a1", AttemptNo: 2,
		OccurredAt: "2026-09-11 08:30:00.000", IngestedAt: "2026-09-11 08:30:05.000",
		ProducerID: "gw-1", TenantID: "t1", AccountID: "acc1", Provider: "anthropic",
		Model: "claude-opus-5", InboundProtocol: "openai_chat", StatusCode: 200,
		LatencyMS: 1234, ErrorClass: "ok", UsageSource: "upstream", Partial: 1,
		TokensIn: 11, TokensOut: 22, CacheReadTokens: 33, CacheWriteTokens: 44,
	}
	if records[0] != want {
		t.Errorf("round trip changed the record:\n got %+v\nwant %+v", records[0], want)
	}
}

// An empty body is an empty result, not a decode error: ClickHouse returns
// nothing at all when no row matches.
func TestDecodeRecordsAcceptsAnEmptyBody(t *testing.T) {
	records, err := decodeRecords(strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Errorf("decoded %d records from an empty body", len(records))
	}
}
