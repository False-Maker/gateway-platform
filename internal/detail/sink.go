package detail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/elucid/gateway-platform/internal/observability"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

// Writer is the transport a Sink flushes batches through. It exists so the
// buffering and drop behaviour -- the part that protects metering -- can be
// tested without ClickHouse, and so a deployment can swap the transport without
// touching the guarantee.
type Writer interface {
	WriteBatch(ctx context.Context, records []Record) error
}

// Sink accepts detail records without ever blocking, failing, or slowing the
// caller down.
//
// A12's DoD: "写入路径与 usage_ledger 事务解耦，明细写失败不得影响计量与 ACK".
// That is enforced here by shape, not by discipline. Observe does a
// non-blocking send onto a bounded channel and returns nothing -- there is no
// error for a caller to accidentally propagate into the ledger transaction,
// and a full buffer or a dead ClickHouse costs the caller one channel select.
// Detail is the thing that gets dropped; metering is the thing that survives.
type Sink struct {
	Writer  Writer
	Metrics *observability.Registry

	// BatchSize and FlushInterval bound how long a record waits before it is
	// visible. Not calibrated against real traffic -- see
	// docs/A12-REQUEST-DETAIL.md.
	BatchSize     int
	FlushInterval time.Duration
	// BufferSize bounds memory. Once full, records are dropped rather than
	// queued: an unbounded queue in front of a store that is allowed to fail
	// just converts a detail outage into a control-plane OOM.
	BufferSize int
	// WriteTimeout bounds one flush.
	WriteTimeout time.Duration

	queue    chan Record
	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	// flushed is signalled after every flush attempt so tests can wait for a
	// batch instead of sleeping.
	flushed chan struct{}
}

const (
	defaultBatchSize     = 500
	defaultFlushInterval = 2 * time.Second
	defaultBufferSize    = 10000
	defaultWriteTimeout  = 5 * time.Second
)

// Start launches the background batcher. A Sink that was never started still
// accepts Observe calls and drops them, so a half-configured deployment
// degrades to "no detail" rather than to a panic on the metering path.
func (s *Sink) Start() {
	if s.BatchSize <= 0 {
		s.BatchSize = defaultBatchSize
	}
	if s.FlushInterval <= 0 {
		s.FlushInterval = defaultFlushInterval
	}
	if s.BufferSize <= 0 {
		s.BufferSize = defaultBufferSize
	}
	if s.WriteTimeout <= 0 {
		s.WriteTimeout = defaultWriteTimeout
	}
	s.queue = make(chan Record, s.BufferSize)
	s.done = make(chan struct{})
	s.flushed = make(chan struct{}, 1)
	s.wg.Add(1)
	go s.loop()
}

// Observe records one release. It never blocks and never returns an error;
// that is the contract that keeps it safe to call from the stream consumer.
// A nil Sink is a working "detail disabled" configuration.
func (s *Sink) Observe(release contracts.Release) {
	if s == nil || s.queue == nil {
		return
	}
	select {
	case s.queue <- FromRelease(release, time.Now().UTC()):
		s.metrics().AddCounter("detail_records_total", 1)
		s.metrics().SetGauge("detail_buffer_depth", float64(len(s.queue)))
	default:
		// The buffer is full: ClickHouse is down or slower than the traffic.
		// Dropping is the designed outcome, but it has to be visible, because
		// silent detail loss looks exactly like silent traffic loss.
		s.metrics().AddCounter("detail_dropped_total", 1, "reason", "buffer_full")
	}
}

func (s *Sink) loop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.FlushInterval)
	defer ticker.Stop()
	batch := make([]Record, 0, s.BatchSize)
	for {
		select {
		case record := <-s.queue:
			batch = append(batch, record)
			if len(batch) >= s.BatchSize {
				batch = s.flush(batch)
			}
		case <-ticker.C:
			batch = s.flush(batch)
		case <-s.done:
			// Drain what is already buffered, then stop. This is best effort:
			// a shutdown is allowed to lose detail, and waiting on ClickHouse
			// to drain would hold up the process that owns metering.
			for {
				select {
				case record := <-s.queue:
					batch = append(batch, record)
					if len(batch) >= s.BatchSize {
						batch = s.flush(batch)
					}
					continue
				default:
				}
				break
			}
			s.flush(batch)
			return
		}
	}
}

// flush writes one batch and always returns an empty batch. A failed write
// discards its records on purpose: retrying in memory would grow without bound
// in exactly the situation (ClickHouse down) the bound exists for.
func (s *Sink) flush(batch []Record) []Record {
	if len(batch) == 0 {
		return batch[:0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.WriteTimeout)
	err := s.Writer.WriteBatch(ctx, batch)
	cancel()
	if err != nil {
		s.metrics().AddCounter("detail_dropped_total", float64(len(batch)), "reason", "write_failed")
		s.metrics().AddCounter("detail_flush_failures_total", 1)
	} else {
		s.metrics().AddCounter("detail_written_total", float64(len(batch)))
	}
	s.metrics().SetGauge("detail_buffer_depth", float64(len(s.queue)))
	select {
	case s.flushed <- struct{}{}:
	default:
	}
	return batch[:0]
}

// Stop drains and stops the batcher. Safe to call more than once.
func (s *Sink) Stop() {
	if s == nil || s.done == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.done) })
	s.wg.Wait()
}

// WaitForFlush blocks until the batcher completes a flush attempt, or the
// timeout expires. Test helper: it exists so integration tests can assert on
// what landed in ClickHouse without sleeping for FlushInterval.
func (s *Sink) WaitForFlush(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.flushed:
		return true
	case <-timer.C:
		return false
	}
}

func (s *Sink) metrics() *observability.Registry {
	if s.Metrics != nil {
		return s.Metrics
	}
	return observability.Default
}

// ClickHouseWriter inserts batches over ClickHouse's HTTP interface using
// JSONEachRow.
//
// The HTTP interface is used rather than a native-protocol driver so that
// choosing ClickHouse does not also add a Go driver dependency to a repository
// that has kept its dependency list short. JSONEachRow is a documented, stable
// ingestion format and the batch is already []Record.
type ClickHouseWriter struct {
	// URL is the ClickHouse HTTP endpoint, e.g.
	// http://user:password@host:8123/. Credentials in this URL come from
	// deployment config and are never written into a detail record.
	URL      string
	Database string
	Table    string
	Client   *http.Client
}

const (
	// DefaultDatabase and DefaultTable name the detail store.
	DefaultDatabase = "gateway_detail"
	DefaultTable    = "request_detail"
)

// EnsureSchema applies DDL. Idempotent: every statement is IF NOT EXISTS, so it
// is safe to run on every boot the way migrations.Apply is.
func (w ClickHouseWriter) EnsureSchema(ctx context.Context) error {
	for _, statement := range DDL(w.database(), w.table()) {
		if err := w.exec(ctx, statement, nil); err != nil {
			return fmt.Errorf("apply detail schema: %w", err)
		}
	}
	return nil
}

// WriteBatch inserts records. An error here is reported to the caller (the
// Sink), which counts it and drops the batch; it never travels further.
func (w ClickHouseWriter) WriteBatch(ctx context.Context, records []Record) error {
	if len(records) == 0 {
		return nil
	}
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			return err
		}
	}
	query := fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow", w.database(), w.table())
	return w.exec(ctx, query, &body)
}

// exec runs a statement and discards the response body. Ingestion never needs
// the response; only the status code matters.
func (w ClickHouseWriter) exec(ctx context.Context, query string, body io.Reader) error {
	return w.execInto(ctx, query, body, io.Discard)
}

// execInto runs a statement and copies the response body to out. Reads (as
// opposed to inserts) go through here.
func (w ClickHouseWriter) execInto(ctx context.Context, query string, body io.Reader, out io.Writer) error {
	if strings.TrimSpace(w.URL) == "" {
		return errors.New("clickhouse url is empty")
	}
	endpoint, err := url.Parse(w.URL)
	if err != nil {
		return fmt.Errorf("parse clickhouse url: %w", err)
	}
	user := endpoint.User
	endpoint.User = nil
	parameters := endpoint.Query()
	parameters.Set("query", query)
	endpoint.RawQuery = parameters.Encode()

	if body == nil {
		body = bytes.NewReader(nil)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), body)
	if err != nil {
		return err
	}
	// Credentials go in headers rather than the URL so a logged or errored
	// request line cannot carry them.
	if user != nil {
		password, _ := user.Password()
		request.SetBasicAuth(user.Username(), password)
	}
	response, err := w.client().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return fmt.Errorf("clickhouse returned %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	_, err = io.Copy(out, response.Body)
	return err
}

func (w ClickHouseWriter) database() string {
	if w.Database != "" {
		return w.Database
	}
	return DefaultDatabase
}

func (w ClickHouseWriter) table() string {
	if w.Table != "" {
		return w.Table
	}
	return DefaultTable
}

func (w ClickHouseWriter) client() *http.Client {
	if w.Client != nil {
		return w.Client
	}
	return http.DefaultClient
}
