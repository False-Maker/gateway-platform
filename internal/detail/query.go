package detail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// This file is the read side of A12's detail store, added for B5's console.
// It lives in this package rather than in the console for the same reason the
// record shape does: the closed field list is the guarantee that no credential
// can reach detail, and the guarantee is only meaningful if there is exactly
// one place that defines what a detail row is. A console that wrote its own
// SELECT would be free to name a column that does not exist here.
//
// Querying detail is explicitly *not* a metering path. 总览 §6.4 keeps detail
// droppable, so a read failure is a console error and never a control-plane
// error; the billing authority stays usage_ledger.

// MaxQueryLimit bounds one console page. Enforced here rather than trusted from
// the caller -- the limit is the one part of the statement that cannot be a
// bound parameter, so it is clamped and then rendered as an integer literal.
const MaxQueryLimit = 500

// QueryFilter selects detail rows. Every field is optional; the zero value
// means "everything, most recent first, one page".
type QueryFilter struct {
	TenantID string
	Provider string
	Start    time.Time
	End      time.Time
	Limit    int
}

// Query returns detail records matching the filter, newest first.
//
// The tenant and window are bound as ClickHouse parameters, so a tenant id
// containing a quote is a value, not SQL. Only the limit is rendered into the
// statement, and only after being clamped to [1, MaxQueryLimit].
func (w ClickHouseWriter) Query(ctx context.Context, filter QueryFilter) ([]Record, error) {
	if strings.TrimSpace(w.URL) == "" {
		return nil, ErrStoreDisabled
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > MaxQueryLimit {
		limit = MaxQueryLimit
	}

	var where []string
	values := url.Values{}
	if filter.TenantID != "" {
		where = append(where, "tenant_id = {tenant:String}")
		values.Set("tenant", filter.TenantID)
	}
	if filter.Provider != "" {
		where = append(where, "provider = {provider:String}")
		values.Set("provider", filter.Provider)
	}
	if !filter.Start.IsZero() {
		where = append(where, "occurred_at >= {start:DateTime64(3)}")
		values.Set("start", filter.Start.UTC().Format(clickHouseTimestamp))
	}
	if !filter.End.IsZero() {
		where = append(where, "occurred_at < {end:DateTime64(3)}")
		values.Set("end", filter.End.UTC().Format(clickHouseTimestamp))
	}
	statement := fmt.Sprintf(`SELECT %s FROM %s.%s`,
		strings.Join(recordColumns, ", "), w.database(), w.table())
	if len(where) > 0 {
		statement += " WHERE " + strings.Join(where, " AND ")
	}
	statement += fmt.Sprintf(" ORDER BY occurred_at DESC, event_id LIMIT %d FORMAT JSONEachRow", limit)

	var body bytes.Buffer
	if err := w.execIntoParams(ctx, statement, values, nil, &body); err != nil {
		return nil, err
	}
	return decodeRecords(&body)
}

// ErrStoreDisabled is returned when no detail store is configured. It is a
// distinct error because "detail is off" is a supported deployment and the
// console reports it as such rather than as a failure.
var ErrStoreDisabled = fmt.Errorf("request detail store is not configured")

// recordColumns is the SELECT list, derived from Record's own json tags rather
// than typed out again. Those tags are already the contract for the INSERT
// side (JSONEachRow marshals the struct), so deriving the read side from the
// same source means a field can never be written but not read, or named
// differently in the two directions. TestRecordColumnsMatchTheStruct holds it
// to that.
var recordColumns = recordColumnsFromStruct()

func recordColumnsFromStruct() []string {
	recordType := reflect.TypeOf(Record{})
	columns := make([]string, 0, recordType.NumField())
	for i := 0; i < recordType.NumField(); i++ {
		tag := recordType.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			panic(fmt.Sprintf("detail.Record field %s has no json tag; the read and insert paths cannot agree on a column name", recordType.Field(i).Name))
		}
		columns = append(columns, name)
	}
	return columns
}

// decodeRecords reads JSONEachRow output. ClickHouse renders DateTime64 as
// "2006-01-02 15:04:05.000" in UTC because that is how the column is declared,
// so the JSON is read into a shadow struct with string timestamps and
// normalised into the same form FromRelease writes.
func decodeRecords(body io.Reader) ([]Record, error) {
	decoder := json.NewDecoder(body)
	var records []Record
	for {
		var row queryRow
		if err := decoder.Decode(&row); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode detail row: %w", err)
		}
		records = append(records, row.record())
	}
	return records, nil
}

// queryRow mirrors the ClickHouse column types on the way in. It differs from
// Record only in how timestamps arrive, which is why the two are kept next to
// each other.
type queryRow struct {
	EventID    string `json:"event_id"`
	RequestID  string `json:"request_id"`
	AttemptID  string `json:"attempt_id"`
	AttemptNo  int    `json:"attempt_no"`
	OccurredAt string `json:"occurred_at"`
	IngestedAt string `json:"ingested_at"`

	ProducerID      string `json:"producer_id"`
	TenantID        string `json:"tenant_id"`
	AccountID       string `json:"account_id"`
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	InboundProtocol string `json:"inbound_protocol"`

	StatusCode       int    `json:"status_code"`
	LatencyMS        int    `json:"latency_ms"`
	ErrorClass       string `json:"error_class"`
	UsageSource      string `json:"usage_source"`
	Partial          uint8  `json:"partial"`
	TokensIn         int    `json:"tokens_in"`
	TokensOut        int    `json:"tokens_out"`
	CacheReadTokens  int    `json:"cache_read_tokens"`
	CacheWriteTokens int    `json:"cache_write_tokens"`
}

func (r queryRow) record() Record {
	return Record{
		EventID: r.EventID, RequestID: r.RequestID, AttemptID: r.AttemptID, AttemptNo: r.AttemptNo,
		OccurredAt: r.OccurredAt, IngestedAt: r.IngestedAt,
		ProducerID: r.ProducerID, TenantID: r.TenantID, AccountID: r.AccountID,
		Provider: r.Provider, Model: r.Model, InboundProtocol: r.InboundProtocol,
		StatusCode: r.StatusCode, LatencyMS: r.LatencyMS, ErrorClass: r.ErrorClass,
		UsageSource: r.UsageSource, Partial: r.Partial,
		TokensIn: r.TokensIn, TokensOut: r.TokensOut,
		CacheReadTokens: r.CacheReadTokens, CacheWriteTokens: r.CacheWriteTokens,
	}
}

// ParseLimit reads the `limit` query parameter, rejecting anything that is not
// a positive integer rather than silently falling back to the default.
func ParseLimit(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 {
		return 0, fmt.Errorf("limit %q is not a positive integer", raw)
	}
	return limit, nil
}
