package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elucid/gateway-platform/internal/events"
	"github.com/elucid/gateway-platform/internal/snapshot"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

func TestStickyIsTenantScopedAndDegradesWithoutRedis(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	ctx := context.Background()
	sticky := Sticky{Redis: rdb}
	sticky.Pin(ctx, "tenant-a", "conv-1", "acc-1", 0)
	if got := sticky.Lookup(ctx, "tenant-a", "conv-1"); got != "acc-1" {
		t.Fatalf("pin lookup=%q", got)
	}
	if got := sticky.Lookup(ctx, "tenant-b", "conv-1"); got != "" {
		t.Fatalf("session key leaked across tenants: %q", got)
	}
	for _, key := range mini.Keys() {
		if strings.Contains(key, "conv-1") {
			t.Fatalf("raw session key stored in Redis: %s", key)
		}
		if ttl := mini.TTL(key); ttl <= 0 || ttl > defaultStickyTTL {
			t.Fatalf("pin ttl=%v", ttl)
		}
	}
	sticky.Pin(ctx, "tenant-a", "conv-1", "acc-2", 30*time.Second)
	if got := sticky.Lookup(ctx, "tenant-a", "conv-1"); got != "acc-2" {
		t.Fatalf("re-pin lookup=%q", got)
	}
	mini.FastForward(31 * time.Second)
	if got := sticky.Lookup(ctx, "tenant-a", "conv-1"); got != "" {
		t.Fatalf("pin survived TTL: %q", got)
	}
	none := Sticky{}
	none.Pin(ctx, "tenant-a", "conv-1", "acc-1", 0)
	if got := none.Lookup(ctx, "tenant-a", "conv-1"); got != "" {
		t.Fatalf("zero Sticky must be inert: %q", got)
	}
	if _, err := normalizeSessionKey(strings.Repeat("x", maxSessionKeyLen+1)); err == nil {
		t.Fatal("oversized session key accepted")
	}
	if _, err := normalizeSessionKey("ok\x00"); err == nil {
		t.Fatal("control character accepted")
	}
	if key, err := normalizeSessionKey("  conv 42 "); err != nil || key != "conv 42" {
		t.Fatalf("normalize=%q err=%v", key, err)
	}
}

func TestChooserPrefersPinnedAccountOnlyWhenEligible(t *testing.T) {
	now := time.Unix(500, 0)
	chooser := NewChooser(func() time.Time { return now })
	chooser.Replace([]contracts.Account{
		{ID: "a", Provider: "x", Platform: "x", Group: "default", Status: "active"},
		{ID: "b", Provider: "x", Platform: "x", Group: "default", Status: "active"},
		{ID: "c", Provider: "x", Platform: "x", Group: "default", Status: "active", ExcludedModels: []string{"m"}},
	})
	criteria := contracts.Criteria{Platform: "x", Group: "default", Model: "m"}
	if lease, _ := chooser.AcquirePreferring(criteria, "", nil); lease.AccountID != "a" {
		t.Fatalf("default order changed: %s", lease.AccountID)
	}
	if lease, _ := chooser.AcquirePreferring(criteria, "b", nil); lease.AccountID != "b" {
		t.Fatalf("pin ignored: %s", lease.AccountID)
	}
	if lease, _ := chooser.AcquirePreferring(criteria, "c", nil); lease.AccountID != "a" {
		t.Fatalf("pin to excluded account honoured: %s", lease.AccountID)
	}
	if lease, _ := chooser.AcquirePreferring(criteria, "ghost", nil); lease.AccountID != "a" {
		t.Fatalf("pin to unknown account honoured: %s", lease.AccountID)
	}
	chooser.MarkFailure("b", "m", contracts.ErrorRateLimitedKnown, time.Minute)
	if lease, _ := chooser.AcquirePreferring(criteria, "b", nil); lease.AccountID != "a" {
		t.Fatalf("pin to cooling account honoured: %s", lease.AccountID)
	}
	if lease, _ := chooser.AcquirePreferring(criteria, "a", map[string]struct{}{"a": {}}); lease.AccountID != "" {
		t.Fatalf("pin overrode failover exclusion: %s", lease.AccountID)
	}
}

func TestTenantLimiterIsIndependentOfAccountLimits(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	ctx := context.Background()
	limiter := Limiter{Redis: rdb}
	if release, err := limiter.AcquireTenant(ctx, "t", TenantLimits{}); err != nil || release == nil {
		t.Fatalf("unlimited tenant: %v", err)
	}
	if len(mini.Keys()) != 0 {
		t.Fatalf("unlimited tenant touched Redis: %v", mini.Keys())
	}
	first, err := limiter.AcquireTenant(ctx, "t", TenantLimits{MaxConcurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limiter.AcquireTenant(ctx, "t", TenantLimits{MaxConcurrency: 1}); !errors.Is(err, ErrTenantRateLimited) {
		t.Fatalf("second concurrent tenant request: %v", err)
	}
	// a different tenant and the account limiter are untouched
	if release, err := limiter.AcquireTenant(ctx, "other", TenantLimits{MaxConcurrency: 1}); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	if release, err := limiter.Acquire(ctx, contracts.Lease{AccountID: "acc", Limits: contracts.AccountLimits{MaxConcurrency: 1}}); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	first()
	if release, err := limiter.AcquireTenant(ctx, "t", TenantLimits{MaxConcurrency: 1}); err != nil {
		t.Fatalf("after release: %v", err)
	} else {
		release()
	}
	// RPM: 2 per minute, third is rejected and reported as a tenant limit
	for i := 0; i < 2; i++ {
		release, err := limiter.AcquireTenant(ctx, "rpm", TenantLimits{RPM: 2})
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if _, err := limiter.AcquireTenant(ctx, "rpm", TenantLimits{RPM: 2}); !errors.Is(err, ErrTenantRateLimited) {
		t.Fatalf("rpm ceiling: %v", err)
	}
	// tenant limits fail closed without Redis
	if _, err := (Limiter{}).AcquireTenant(ctx, "t", TenantLimits{RPM: 1}); err == nil {
		t.Fatal("tenant limit was lifted when Redis is unavailable")
	}
}

func TestRouterAppliesStickySessionsAndTenantLimits(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	ctx := context.Background()
	var mu sync.Mutex
	var releaseUpstream chan struct{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gate := releaseUpstream
		mu.Unlock()
		if gate != nil {
			<-gate
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()
	accounts := []contracts.Account{
		{ID: "acc-1", Provider: "grok", Platform: "grok", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "k", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "openai_chat"}},
		{ID: "acc-2", Provider: "grok", Platform: "grok", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "k", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "openai_chat"}},
	}
	if _, err := (snapshot.Publisher{Redis: rdb}).Publish(ctx, "grok", "default", 0, accounts); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.PublishTokens(ctx, rdb, map[string]snapshot.TokenRecord{
		snapshot.HashToken("gwt_free"):    {TokenID: "t1", TenantID: "tenant-free", PrincipalID: "p", Group: "default"},
		snapshot.HashToken("gwt_limited"): {TokenID: "t2", TenantID: "tenant-limited", PrincipalID: "p", Group: "default", MaxConcurrency: 1, RPM: 3},
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
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	call := func(token, session string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+token)
		if session != "" {
			request.Header.Set(SessionKeyHeader, session)
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder
	}

	// Sticky: pin the session to acc-2 (not the deterministic first choice) and
	// verify the conversation keeps landing there; another session and another
	// tenant are unaffected.
	server.Sticky.Pin(ctx, "tenant-free", "conv-A", "acc-2", 0)
	for i := 0; i < 3; i++ {
		if rec := call("gwt_free", "conv-A"); rec.Code != http.StatusOK {
			t.Fatalf("sticky call %d: %d %s", i, rec.Code, rec.Body.String())
		}
		if release := latestRelease(t, rdb); release.AccountID != "acc-2" {
			t.Fatalf("sticky session left acc-2: %+v", release)
		}
	}
	if rec := call("gwt_free", "conv-B"); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if release := latestRelease(t, rdb); release.AccountID != "acc-1" {
		t.Fatalf("unpinned session did not use default order: %+v", release)
	}
	if got := server.Sticky.Lookup(ctx, "tenant-free", "conv-B"); got != "acc-1" {
		t.Fatalf("first request did not pin its session: %q", got)
	}
	if rec := call("gwt_limited", "conv-A"); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if release := latestRelease(t, rdb); release.AccountID != "acc-1" {
		t.Fatalf("session key crossed tenants: %+v", release)
	}
	if rec := call("gwt_free", strings.Repeat("s", maxSessionKeyLen+1)); rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized session key: %d", rec.Code)
	}

	// Tenant concurrency: hold one request open on the limited tenant, the
	// second is 429 while the free tenant still passes.
	mu.Lock()
	releaseUpstream = make(chan struct{})
	mu.Unlock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call("gwt_limited", "") }()
	deadline := time.Now().Add(2 * time.Second)
	for rdb.Exists(ctx, "gateway:tenant:concurrency:tenant-limited").Val() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first limited request never acquired the tenant slot")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rec := call("gwt_limited", ""); rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "tenant") {
		t.Fatalf("tenant concurrency not enforced: %d %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	gate := releaseUpstream
	releaseUpstream = nil
	mu.Unlock()
	if rec := call("gwt_free", ""); rec.Code != http.StatusOK {
		t.Fatalf("free tenant blocked by limited tenant: %d", rec.Code)
	}
	close(gate)
	if rec := <-done; rec.Code != http.StatusOK {
		t.Fatalf("held request: %d %s", rec.Code, rec.Body.String())
	}
	// Tenant RPM: 3 per minute for the limited tenant; it already used 2
	// (one sticky call, one held call), so one more passes and the next is 429.
	if rec := call("gwt_limited", ""); rec.Code != http.StatusOK {
		t.Fatalf("third rpm call: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call("gwt_limited", ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("rpm ceiling not enforced: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call("gwt_free", ""); rec.Code != http.StatusOK {
		t.Fatalf("free tenant hit by other tenant's rpm: %d", rec.Code)
	}
}
