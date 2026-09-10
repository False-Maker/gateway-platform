package control

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/elucid/gateway-platform/internal/detail"
	"github.com/elucid/gateway-platform/internal/observability"
	"github.com/elucid/gateway-platform/migrations"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

// brokenDetailWriter is a detail store that is always down.
type brokenDetailWriter struct{ calls chan struct{} }

func (w brokenDetailWriter) WriteBatch(context.Context, []detail.Record) error {
	select {
	case w.calls <- struct{}{}:
	default:
	}
	return errors.New("clickhouse is unreachable")
}

// A12's load-bearing claim, tested against real PostgreSQL: when the request
// detail store is completely unavailable, the metering row is still written and
// HandleRelease still returns nil -- which is what lets the stream consumer ACK.
// If this test ever fails, detail has become a dependency of billing.
func TestDetailFailureDoesNotAffectMeteringOrAck(t *testing.T) {
	databaseURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := migrations.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}

	const attemptID = "a12-attempt"
	const tenantID = "a12-detail-tenant"
	cleanup := func() {
		background := context.Background()
		db.Exec(background, `DELETE FROM usage_ledger WHERE attempt_id=$1`, attemptID)
		db.Exec(background, `DELETE FROM request_attempts WHERE attempt_id=$1`, attemptID)
	}
	cleanup()
	t.Cleanup(cleanup)

	writer := brokenDetailWriter{calls: make(chan struct{}, 1)}
	sink := &detail.Sink{Writer: writer, Metrics: observability.NewRegistry(),
		BatchSize: 1, FlushInterval: 10 * time.Millisecond}
	sink.Start()
	defer sink.Stop()

	ledger := Ledger{DB: db, Metrics: observability.NewRegistry(), Detail: sink}
	startedAt := time.Now().UTC()
	if err := ledger.HandleAttemptStarted(ctx, contracts.AttemptStarted{
		SchemaVersion: contracts.SchemaVersion, EventID: "a12-start", RequestID: "a12-request",
		AttemptID: attemptID, AttemptNo: 1, ProducerID: "a12-producer", OccurredAt: startedAt,
		AccountID: "a12-account", Provider: "anthropic", Model: "claude-opus-5", TenantID: tenantID,
		DeadlineAt: startedAt.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	release := contracts.Release{
		SchemaVersion: contracts.SchemaVersion, EventID: contracts.TerminalEventID(attemptID),
		RequestID: "a12-request", AttemptID: attemptID, AttemptNo: 1, ProducerID: "a12-producer",
		OccurredAt: startedAt, AccountID: "a12-account", Provider: "anthropic",
		StatusCode: 200, LatencyMS: 42, Model: "claude-opus-5", TenantID: tenantID,
		ErrorClass: contracts.ErrorOK, UsageSource: contracts.UsageSourceUpstream,
		TokensIn: 7, TokensOut: 9, InboundProtocol: "openai_chat",
	}
	// The whole assertion: no error, therefore the consumer ACKs.
	if err := ledger.HandleRelease(ctx, release); err != nil {
		t.Fatalf("a dead detail store broke metering: %v", err)
	}

	var tokensIn, tokensOut int
	if err := db.QueryRow(ctx,
		`SELECT tokens_in, tokens_out FROM usage_ledger WHERE attempt_id=$1`, attemptID).
		Scan(&tokensIn, &tokensOut); err != nil {
		t.Fatalf("the metering row is missing after a detail failure: %v", err)
	}
	if tokensIn != 7 || tokensOut != 9 {
		t.Errorf("metering row = %d/%d, want 7/9", tokensIn, tokensOut)
	}

	// And confirm the sink really did try -- otherwise this test would pass
	// just as well with detail wired to nothing at all.
	select {
	case <-writer.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("the detail sink never attempted a write, so the failure path was not exercised")
	}
}
