package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elucid/gateway-platform/internal/control"
	"github.com/elucid/gateway-platform/internal/control/credentials"
	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/internal/control/provider/codex"
	"github.com/elucid/gateway-platform/internal/events"
	"github.com/elucid/gateway-platform/internal/snapshot"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// TestProcessE2EControlGatewayRelease exercises the process boundary using a
// local provider. PostgreSQL remains optional so the normal no-credential test
// suite can run without external infrastructure.
func TestProcessE2EControlGatewayRelease(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("GATEWAY_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Clean(filepath.Join(sourceDir(t), "../.."))
	migration, err := os.ReadFile(filepath.Join(repoRoot, "migrations", "001_initial.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}

	const sourceSystem = "a4-process-e2e"
	const sourceID = "codex-local"
	accountID := importedAccountIDForTest(sourceSystem, sourceID)
	if err := cleanupProcessE2EAccount(ctx, db, accountID); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanupProcessE2EAccount(context.Background(), db, accountID) }()

	key := bytes.Repeat([]byte{0x41}, 32)
	cipher, err := credentials.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	upstreamHits := atomic.Int32{}
	upstreamErrs := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			recordProcessE2EError(upstreamErrs, fmt.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path))
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer fixture-key" {
			recordProcessE2EError(upstreamErrs, fmt.Errorf("unexpected upstream authorization %q", got))
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			recordProcessE2EError(upstreamErrs, fmt.Errorf("decode upstream request: %w", err))
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if request.Model != "fixture-model" {
			recordProcessE2EError(upstreamErrs, fmt.Errorf("unexpected upstream model %q", request.Model))
			http.Error(w, "invalid model", http.StatusBadRequest)
			return
		}
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"fixture-response","object":"chat.completion","model":"fixture-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello from fixture"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`)
	}))
	defer upstream.Close()

	provider := codex.NewAPIKey(nil)
	lookup := func(kind string) (providerapi.Provider, bool) { return provider, kind == providerapi.KindCodex }
	request := contracts.ImportRequest{SourceSystem: sourceSystem, SourceID: sourceID, Provider: providerapi.KindCodex, AuthMode: providerapi.AuthModeAPIKey, StaticKey: "fixture-key", Metadata: map[string]string{"group": "default"}}
	account, err := (control.ImportService{Repository: control.PGImportRepository{DB: db}, Cipher: cipher, Provider: lookup}).Import(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != accountID || account.Credential.AccessToken != "fixture-key" {
		t.Fatalf("unexpected imported account: %#v", account)
	}
	profile := contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "openai_chat", InferencePath: "/v1/chat/completions"}
	profileJSON, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE accounts SET base_url=$1, profile=$2::jsonb WHERE id=$3`, upstream.URL, string(profileJSON), accountID); err != nil {
		t.Fatal(err)
	}

	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer rdb.Close()
	controlMetrics := freeTCPAddress(t)
	gatewayListen := freeTCPAddress(t)
	binary := buildGWDBinary(t, repoRoot)
	credentialKey := base64.StdEncoding.EncodeToString(key)
	controlProcess := startProcess(t, binary, processEnv(map[string]string{
		"GATEWAY_DATABASE_URL":   databaseURL,
		"GATEWAY_CREDENTIAL_KEY": credentialKey,
		"GATEWAY_REDIS_ADDR":     mini.Addr(),
		"GATEWAY_REDIS_USERNAME": "",
		"GATEWAY_REDIS_PASSWORD": "",
		"GATEWAY_CONSUMER_ID":    "a4-control",
		"GATEWAY_METRICS_ADDR":   controlMetrics,
	}), "--role", "control")
	defer controlProcess.stop()

	loader := snapshot.Loader{Redis: rdb}
	waitForProcessE2E(t, 15*time.Second, func() bool {
		loaded, err := loader.Load(ctx, providerapi.KindCodex, "default")
		return err == nil && len(loaded.Accounts) == 1 && loaded.Accounts[0].ID == accountID
	})

	gatewayProcess := startProcess(t, binary, processEnv(map[string]string{
		"GATEWAY_REDIS_ADDR":     mini.Addr(),
		"GATEWAY_REDIS_USERNAME": "",
		"GATEWAY_REDIS_PASSWORD": "",
		"GATEWAY_LISTEN_ADDR":    gatewayListen,
		"GATEWAY_PRODUCER_ID":    "a4-gateway",
		"GATEWAY_TENANT_ID":      "a4-tenant",
		"GATEWAY_PROVIDER":       providerapi.KindCodex,
	}), "--role", "gateway")
	defer gatewayProcess.stop()
	waitForProcessE2E(t, 15*time.Second, func() bool {
		response, err := http.Get("http://" + gatewayListen + "/metrics")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK
	})

	requestBody := []byte(`{"model":"fixture-model","messages":[{"role":"user","content":"hello"}]}`)
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Post("http://"+gatewayListen+"/v1/chat/completions", "application/json", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	responseBody, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s\ncontrol output=%s\ngateway output=%s", response.StatusCode, responseBody, controlProcess.output(), gatewayProcess.output())
	}
	if !bytes.Contains(responseBody, []byte("hello from fixture")) {
		t.Fatalf("gateway response did not contain provider content: %s", responseBody)
	}
	if got := upstreamHits.Load(); got != 1 {
		t.Fatalf("expected one upstream request, got %d", got)
	}
	select {
	case err := <-upstreamErrs:
		t.Fatal(err)
	default:
	}

	row := waitForProcessE2ELedger(t, db, accountID)
	if row.StatusCode != http.StatusOK || contracts.ErrorClass(row.ErrorClass) != contracts.ErrorOK || row.TokensIn != 3 || row.TokensOut != 5 || row.UsageSource != contracts.UsageSourceUpstream || row.Partial {
		t.Fatalf("unexpected usage ledger row: %#v", row)
	}
	var state string
	if err := db.QueryRow(ctx, `SELECT state FROM request_attempts WHERE attempt_id=$1`, row.AttemptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "terminal" {
		t.Fatalf("attempt state=%q, want terminal", state)
	}

	duplicate := contracts.Release{SchemaVersion: contracts.SchemaVersion, EventID: row.EventID, RequestID: row.RequestID, AttemptID: row.AttemptID, AttemptNo: 1, ProducerID: "a4-gateway", OccurredAt: row.OccurredAt, AccountID: row.AccountID, Provider: row.Provider, StatusCode: row.StatusCode, Model: row.Model, TenantID: row.TenantID, ErrorClass: contracts.ErrorClass(row.ErrorClass), UsageSource: row.UsageSource, Partial: row.Partial, TokensIn: row.TokensIn, TokensOut: row.TokensOut}
	duplicateEvent, err := contracts.NewStreamEvent(contracts.EventTypeRelease, duplicate.EventID, duplicate.RequestID, duplicate.AttemptID, duplicate.ProducerID, duplicate.OccurredAt, duplicate)
	if err != nil {
		t.Fatal(err)
	}
	duplicateStreamID, err := (events.Producer{Redis: rdb, ProducerID: "a4-gateway"}).Add(ctx, duplicateEvent)
	if err != nil {
		t.Fatal(err)
	}
	waitForProcessE2E(t, 5*time.Second, func() bool {
		groups, err := rdb.XInfoGroups(ctx, events.StreamKey).Result()
		if err != nil || len(groups) != 1 {
			return false
		}
		return groups[0].LastDeliveredID == duplicateStreamID && groups[0].Pending == 0
	})
	var ledgerCount, attemptCount int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM usage_ledger WHERE account_id=$1 AND provider=$2 AND model=$3 AND tenant_id=$4`, accountID, providerapi.KindCodex, "fixture-model", "a4-tenant").Scan(&ledgerCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM request_attempts WHERE account_id=$1 AND provider=$2 AND model=$3 AND tenant_id=$4`, accountID, providerapi.KindCodex, "fixture-model", "a4-tenant").Scan(&attemptCount); err != nil {
		t.Fatal(err)
	}
	if ledgerCount != 1 || attemptCount != 1 {
		t.Fatalf("duplicate release changed counts: usage_ledger=%d request_attempts=%d", ledgerCount, attemptCount)
	}
}

type processE2ELedgerRow struct {
	EventID     string
	RequestID   string
	AttemptID   string
	TenantID    string
	AccountID   string
	Provider    string
	Model       string
	StatusCode  int
	ErrorClass  string
	TokensIn    int
	TokensOut   int
	UsageSource string
	Partial     bool
	OccurredAt  time.Time
}

func waitForProcessE2ELedger(t *testing.T, db *pgxpool.Pool, accountID string) processE2ELedgerRow {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := db.QueryRow(context.Background(), `SELECT count(*) FROM usage_ledger WHERE account_id=$1 AND provider=$2 AND model=$3 AND tenant_id=$4`, accountID, providerapi.KindCodex, "fixture-model", "a4-tenant").Scan(&count); err == nil {
			if count > 1 {
				t.Fatalf("usage ledger has %d rows, want exactly one", count)
			}
			if count == 1 {
				var row processE2ELedgerRow
				err := db.QueryRow(context.Background(), `SELECT event_id,request_id,attempt_id,tenant_id,account_id,provider,model,status_code,error_class,tokens_in,tokens_out,usage_source,partial,occurred_at FROM usage_ledger WHERE account_id=$1 AND provider=$2 AND model=$3 AND tenant_id=$4`, accountID, providerapi.KindCodex, "fixture-model", "a4-tenant").Scan(&row.EventID, &row.RequestID, &row.AttemptID, &row.TenantID, &row.AccountID, &row.Provider, &row.Model, &row.StatusCode, &row.ErrorClass, &row.TokensIn, &row.TokensOut, &row.UsageSource, &row.Partial, &row.OccurredAt)
				if err != nil {
					t.Fatal(err)
				}
				return row
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for usage ledger")
	return processE2ELedgerRow{}
}

func cleanupProcessE2EAccount(ctx context.Context, db *pgxpool.Pool, accountID string) error {
	if _, err := db.Exec(ctx, `DELETE FROM usage_ledger WHERE account_id=$1`, accountID); err != nil {
		return err
	}
	if _, err := db.Exec(ctx, `DELETE FROM request_attempts WHERE account_id=$1`, accountID); err != nil {
		return err
	}
	_, err := db.Exec(ctx, `DELETE FROM accounts WHERE id=$1`, accountID)
	return err
}

func buildGWDBinary(t *testing.T, repoRoot string) string {
	t.Helper()
	goBinary := "/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go"
	if _, err := os.Stat(goBinary); err != nil {
		t.Skipf("Go toolchain is unavailable: %v", err)
	}
	path := filepath.Join(t.TempDir(), "gwd")
	command := exec.Command(goBinary, "build", "-o", path, "./cmd/gwd")
	command.Dir = repoRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build gwd: %v\n%s", err, output)
	}
	return path
}

type e2eProcess struct {
	cmd    *exec.Cmd
	stdout synchronizedBuffer
	stderr synchronizedBuffer
}

func startProcess(t *testing.T, binary string, env []string, args ...string) *e2eProcess {
	t.Helper()
	process := &e2eProcess{cmd: exec.Command(binary, args...)}
	process.cmd.Env = env
	process.cmd.Stdout = &process.stdout
	process.cmd.Stderr = &process.stderr
	if err := process.cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", args, err)
	}
	return process
}

func (p *e2eProcess) output() string {
	return p.stdout.String() + p.stderr.String()
}

type synchronizedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *synchronizedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (p *e2eProcess) stop() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_, _ = p.cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = p.cmd.Process.Kill()
		<-done
	}
}

func processEnv(overrides map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, item := range os.Environ() {
		key, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if _, replace := overrides[key]; !replace {
			env = append(env, item)
		}
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

func freeTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}

func waitForProcessE2E(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition was not satisfied within %s", timeout)
}

func recordProcessE2EError(channel chan<- error, err error) {
	select {
	case channel <- err:
	default:
	}
}

func sourceDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test source directory")
	}
	return filepath.Dir(file)
}

func importedAccountIDForTest(sourceSystem, sourceID string) string {
	// Keep the test's account lookup independent from package-private control
	// helpers while matching ImportService's deterministic ID format.
	digest := sha256.Sum256([]byte(sourceSystem + "\x00" + sourceID))
	return "account-" + hex.EncodeToString(digest[:])[:32]
}
