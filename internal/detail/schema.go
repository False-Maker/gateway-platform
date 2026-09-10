// Package detail implements A12: the request-detail store.
//
// The whole point of this package is separation. 总览 §6.4 makes the physical
// split of metering flow (usage_ledger, control-state PostgreSQL, single
// writer) from high-frequency request detail a P0 decision, because control's
// load is supposed to grow with the number of accounts, not with RPM. Detail
// therefore lives in ClickHouse, is written asynchronously off the single-writer
// path, and is droppable: if this package fails entirely, metering and stream
// ACK must be unaffected. Every design choice below follows from that.
//
// See docs/A12-REQUEST-DETAIL.md for the decision record.
package detail

import (
	"fmt"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

// Record is one request attempt as the detail store sees it.
//
// The field list is closed on purpose. A12's DoD forbids credentials, plaintext
// tokens and refresh tokens from entering detail; the way that is guaranteed
// here is structural rather than by filtering -- there is no free-form map, no
// header bag and no request/response body field for a secret to arrive in.
// Adding one would be the change that breaks the guarantee, which is what
// TestRecordCarriesNoFreeFormFields exists to catch.
type Record struct {
	// Identity: enough to join back to usage_ledger and to the stream event.
	EventID    string `json:"event_id"`
	RequestID  string `json:"request_id"`
	AttemptID  string `json:"attempt_id"`
	AttemptNo  int    `json:"attempt_no"`
	OccurredAt string `json:"occurred_at"`
	IngestedAt string `json:"ingested_at"`

	// Routing: which gateway node served it, for which tenant, on which
	// account and provider, speaking which inbound protocol. Together with
	// attempt_no this is the retry chain -- the reason detail exists at all.
	ProducerID      string `json:"producer_id"`
	TenantID        string `json:"tenant_id"`
	AccountID       string `json:"account_id"`
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	InboundProtocol string `json:"inbound_protocol"`

	// Outcome, mirroring the Release contract.
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

// clickHouseTimestamp formats for DateTime64(3, 'UTC') columns. ClickHouse
// parses this form directly out of JSONEachRow.
const clickHouseTimestamp = "2006-01-02 15:04:05.000"

// FromRelease projects a Release into a detail record. It is a projection and
// nothing more: it never reads the credential store, never touches the request
// body, and cannot fail.
func FromRelease(release contracts.Release, ingestedAt time.Time) Record {
	partial := uint8(0)
	if release.Partial {
		partial = 1
	}
	return Record{
		EventID:    release.EventID,
		RequestID:  release.RequestID,
		AttemptID:  release.AttemptID,
		AttemptNo:  release.AttemptNo,
		OccurredAt: release.OccurredAt.UTC().Format(clickHouseTimestamp),
		IngestedAt: ingestedAt.UTC().Format(clickHouseTimestamp),

		ProducerID:      release.ProducerID,
		TenantID:        release.TenantID,
		AccountID:       release.AccountID,
		Provider:        release.Provider,
		Model:           release.Model,
		InboundProtocol: release.InboundProtocol,

		StatusCode:       release.StatusCode,
		LatencyMS:        release.LatencyMS,
		ErrorClass:       string(release.ErrorClass),
		UsageSource:      release.UsageSource,
		Partial:          partial,
		TokensIn:         release.TokensIn,
		TokensOut:        release.TokensOut,
		CacheReadTokens:  release.CacheReadTokens,
		CacheWriteTokens: release.CacheWriteTokens,
	}
}

// DefaultTTLDays bounds how long detail is kept. Detail is an operational aid,
// not a record of account: the ledger is what has to survive, so detail expires
// on its own rather than growing without limit. It is not calibrated against
// real traffic volume -- see docs/A12-REQUEST-DETAIL.md.
const DefaultTTLDays = 30

// DDL returns the statements that create the detail table, in order.
//
// ReplacingMergeTree keyed by event_id makes a redelivered stream message
// converge instead of double-counting. That matters because the sink is
// at-least-once by construction: it writes after the ledger transaction
// commits, so a consumer restart between commit and flush replays the release.
// Detail must never be summed as if it were the ledger -- the ledger is the
// authority, and ReplacingMergeTree only merges eventually.
func DDL(database, table string) []string {
	return []string{
		fmt.Sprintf(`CREATE DATABASE IF NOT EXISTS %s`, database),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s (
	event_id           String,
	request_id         String,
	attempt_id         String,
	attempt_no         Int32,
	occurred_at        DateTime64(3, 'UTC'),
	ingested_at        DateTime64(3, 'UTC'),
	producer_id        LowCardinality(String),
	tenant_id          LowCardinality(String),
	account_id         String,
	provider           LowCardinality(String),
	model              LowCardinality(String),
	inbound_protocol   LowCardinality(String),
	status_code        Int32,
	latency_ms         Int32,
	error_class        LowCardinality(String),
	usage_source       LowCardinality(String),
	partial            UInt8,
	tokens_in          Int32,
	tokens_out         Int32,
	cache_read_tokens  Int32,
	cache_write_tokens Int32
) ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY toYYYYMMDD(occurred_at)
ORDER BY (tenant_id, occurred_at, event_id)
TTL toDateTime(occurred_at) + INTERVAL %d DAY`, database, table, DefaultTTLDays),
	}
}
