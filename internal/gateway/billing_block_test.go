package gateway

import (
	"bytes"
	"context"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/elucid/gateway-platform/internal/events"
	"github.com/elucid/gateway-platform/internal/snapshot"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

// eventsStreamKeyForTest aliases the release stream key so the assertion below
// can name its result `events` without shadowing the package.
const eventsStreamKeyForTest = events.StreamKey

// A tenant whose wallet control published as exhausted must be refused with 402
// before any upstream work: no upstream request, no Release event, no ledger
// row. A healthy tenant sharing the same router is untouched.
func TestRouterRefusesBillingBlockedTenantsWithoutTouchingUpstream(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	ctx := context.Background()

	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	accounts := []contracts.Account{{
		ID: "acc-1", Provider: "grok", Platform: "grok", Group: "default", Status: "active",
		Credential: contracts.Credential{Kind: "static", AccessToken: "k", Version: 1},
		Profile:    contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "openai_chat"},
	}}
	if _, err := (snapshot.Publisher{Redis: rdb}).Publish(ctx, "grok", "default", 0, accounts); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.PublishTokens(ctx, rdb, map[string]snapshot.TokenRecord{
		snapshot.HashToken("gwt_solvent"): {TokenID: "t1", TenantID: "tenant-solvent", PrincipalID: "p", Group: "default"},
		snapshot.HashToken("gwt_broke"):   {TokenID: "t2", TenantID: "tenant-broke", PrincipalID: "p", Group: "default", BillingBlocked: true},
	}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "apikey")
	server.ChooseAllPlatforms = true
	server.Sticky = Sticky{Redis: rdb}
	if err := (BucketLoader{Redis: rdb, Chooser: server.Chooser}).RefreshAll(ctx); err != nil {
		t.Fatal(err)
	}
	auth := NewAuthenticator(rdb)
	if err := auth.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	router := NewRouter(server, auth)
	call := func(token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			bytes.NewReader([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder
	}

	blocked := call("gwt_broke")
	if blocked.Code != http.StatusPaymentRequired {
		t.Fatalf("blocked tenant got %d %s, want 402", blocked.Code, blocked.Body.String())
	}
	// The refusal has to name the tenant, or an operator staring at a client
	// error has no way to tell which wallet ran dry.
	if !strings.Contains(blocked.Body.String(), "tenant-broke") {
		t.Errorf("402 body does not identify the tenant: %s", blocked.Body.String())
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("a refused request still hit upstream %d times", got)
	}
	// No Release event at all: a refused request never entered the scheduler,
	// so control must have nothing to bill or reconcile for it.
	events, err := rdb.XRange(ctx, eventsStreamKeyForTest, "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("a refused request produced %d events", len(events))
	}
	// 401 is a different failure with a different remedy; it must not collapse
	// into 402 or the client is told to top up a wallet it does not own.
	if unknown := call("gwt_unknown"); unknown.Code != http.StatusUnauthorized {
		t.Errorf("unknown token got %d, want 401", unknown.Code)
	}

	// The block is per tenant, not global.
	if ok := call("gwt_solvent"); ok.Code != http.StatusOK {
		t.Fatalf("solvent tenant got %d %s", ok.Code, ok.Body.String())
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want exactly the solvent tenant's one", got)
	}
}

// The B4.4 DoD requires that the gateway never asks PostgreSQL or control about
// a balance on the hot path. Asserting on the import graph proves it for all
// code paths at once, including ones no test exercises: a future edit that adds
// a PG query or a control call to the gateway fails here even if it is never
// called.
func TestGatewayDoesNotDependOnPostgresOrControl(t *testing.T) {
	const modulePath = "github.com/elucid/gateway-platform/"
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not where this test expects it: %v", err)
	}

	visited := map[string]bool{}
	var walk func(t *testing.T, pkg string)
	walk = func(t *testing.T, pkg string) {
		if visited[pkg] {
			return
		}
		visited[pkg] = true
		dir := filepath.Join(root, strings.TrimPrefix(pkg, modulePath))
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read package %s: %v", pkg, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			// Test files are excluded on purpose: a test may legitimately reach
			// for PostgreSQL, the shipped hot path may not.
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			for _, spec := range file.Imports {
				imported, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(imported, "jackc/pgx") || imported == "database/sql" {
					t.Errorf("%s/%s imports %s: the hot path must not reach PostgreSQL", pkg, name, imported)
				}
				if strings.HasPrefix(imported, modulePath+"internal/control") {
					t.Errorf("%s/%s imports %s: the hot path must not call control", pkg, name, imported)
				}
				if strings.HasPrefix(imported, modulePath) {
					walk(t, imported)
				}
			}
		}
	}
	walk(t, modulePath+"internal/gateway")
	// Guard against the walk silently doing nothing: gateway is known to reach
	// at least its snapshot, events and contracts packages.
	for _, required := range []string{"internal/snapshot", "internal/events", "pkg/contracts"} {
		if !visited[modulePath+required] {
			t.Errorf("import walk never reached %s; the assertion above proved nothing", required)
		}
	}
}
