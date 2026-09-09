package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elucid/gateway-platform/internal/events"
	"github.com/elucid/gateway-platform/internal/snapshot"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

func TestChooserServesMultipleBucketsAndIsolatesGroups(t *testing.T) {
	now := time.Unix(1000, 0)
	chooser := NewChooser(func() time.Time { return now })
	fresh := now.Add(time.Minute)
	chooser.ReplaceBucket("grok", "default", []contracts.Account{{ID: "grok-1", Provider: "grok", Platform: "grok", Group: "default", Status: "active", Capabilities: []string{"grok-model"}}}, 1, fresh)
	chooser.ReplaceBucket("claude", "default", []contracts.Account{{ID: "claude-1", Provider: "claude", Platform: "claude", Group: "default", Status: "active", Capabilities: []string{"claude-model"}}}, 1, fresh)
	chooser.ReplaceBucket("claude", "vip", []contracts.Account{{ID: "claude-vip", Provider: "claude", Platform: "claude", Group: "vip", Status: "active", Capabilities: []string{"claude-model"}}}, 1, fresh)

	lease, err := chooser.Acquire(contracts.Criteria{Model: "grok-model", Group: "default"})
	if err != nil || lease.AccountID != "grok-1" || lease.Provider != "grok" {
		t.Fatalf("model routed to wrong bucket: lease=%#v err=%v", lease, err)
	}
	lease, err = chooser.Acquire(contracts.Criteria{Model: "claude-model", Group: "default"})
	if err != nil || lease.AccountID != "claude-1" || lease.Provider != "claude" {
		t.Fatalf("claude default: lease=%#v err=%v", lease, err)
	}
	// group isolation: vip-only account never leaks into default, and vice versa.
	lease, err = chooser.Acquire(contracts.Criteria{Model: "claude-model", Group: "vip"})
	if err != nil || lease.AccountID != "claude-vip" {
		t.Fatalf("vip group: lease=%#v err=%v", lease, err)
	}
	if _, err := chooser.Acquire(contracts.Criteria{Model: "grok-model", Group: "vip"}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("vip must not see default grok account: %v", err)
	}
	// explicit platform pin still works
	if _, err := chooser.Acquire(contracts.Criteria{Platform: "grok", Model: "claude-model", Group: "default"}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("platform pin ignored: %v", err)
	}

	// epoch bump on one bucket clears only that bucket's cooldowns
	chooser.MarkFailure("grok-1", "grok-model", contracts.ErrorRateLimitedKnown, time.Minute)
	chooser.MarkFailure("claude-1", "claude-model", contracts.ErrorRateLimitedKnown, time.Minute)
	chooser.ReplaceBucket("grok", "default", []contracts.Account{{ID: "grok-1", Provider: "grok", Platform: "grok", Group: "default", Status: "active", Capabilities: []string{"grok-model"}}}, 2, fresh)
	if _, err := chooser.Acquire(contracts.Criteria{Model: "grok-model", Group: "default"}); err != nil {
		t.Fatalf("grok cooldown not cleared by its epoch: %v", err)
	}
	if _, err := chooser.Acquire(contracts.Criteria{Model: "claude-model", Group: "default"}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("claude cooldown wrongly cleared by grok epoch: %v", err)
	}

	// stale bucket is fenced off; removed bucket disappears
	chooser.ReplaceBucket("claude", "vip", []contracts.Account{{ID: "claude-vip", Provider: "claude", Platform: "claude", Group: "vip", Status: "active", Capabilities: []string{"claude-model"}}}, 2, now.Add(-time.Second))
	if _, err := chooser.Acquire(contracts.Criteria{Model: "claude-model", Group: "vip"}); !errors.Is(err, ErrSnapshotStale) {
		t.Fatalf("stale bucket served: %v", err)
	}
	chooser.RemoveBucket("claude", "vip")
	if _, err := chooser.Acquire(contracts.Criteria{Model: "claude-model", Group: "vip"}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("removed bucket still present: %v", err)
	}
	if got := chooser.Buckets(); len(got) != 2 {
		t.Fatalf("buckets=%v", got)
	}
}

func TestBucketLoaderTracksRegistry(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	ctx := context.Background()
	publisher := snapshot.Publisher{Redis: rdb}
	if _, err := publisher.Publish(ctx, "grok", "default", 0, []contracts.Account{{ID: "g", Provider: "grok", Platform: "grok", Group: "default", Status: "active"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Publish(ctx, "claude", "team", 0, []contracts.Account{{ID: "c", Provider: "claude", Platform: "claude", Group: "team", Status: "active"}}); err != nil {
		t.Fatal(err)
	}
	chooser := NewChooser(nil)
	loader := BucketLoader{Redis: rdb, Chooser: chooser}
	if err := loader.RefreshAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := chooser.Buckets(); len(got) != 2 {
		t.Fatalf("buckets=%v", got)
	}
	if lease, err := chooser.Acquire(contracts.Criteria{Model: "m", Group: "team"}); err != nil || lease.AccountID != "c" {
		t.Fatalf("team lease=%#v err=%v", lease, err)
	}
	if err := snapshot.RemoveRegisteredBucket(ctx, rdb, "claude", "team"); err != nil {
		t.Fatal(err)
	}
	if err := loader.RefreshAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := chooser.Acquire(contracts.Criteria{Model: "m", Group: "team"}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("deregistered bucket still served: %v", err)
	}
}

func TestRouterAuthenticatesAndDerivesTenantFromToken(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	// Two buckets, two tenants: tenant A's token is bound to group "a", tenant B's to "b".
	publisher := snapshot.Publisher{Redis: rdb}
	if _, err := publisher.Publish(ctx, "grok", "a", 0, []contracts.Account{{ID: "acc-a", Provider: "grok", Platform: "grok", Group: "a", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "k", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "openai_chat"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Publish(ctx, "grok", "b", 0, []contracts.Account{{ID: "acc-b", Provider: "grok", Platform: "grok", Group: "b", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "k", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "openai_chat"}}}); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.PublishTokens(ctx, rdb, map[string]snapshot.TokenRecord{
		snapshot.HashToken("gwt_a"): {TokenID: "ta", TenantID: "tenant-a", PrincipalID: "pa", Group: "a"},
		snapshot.HashToken("gwt_b"): {TokenID: "tb", TenantID: "tenant-b", PrincipalID: "pb", Group: "b"},
	}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "apikey")
	server.ChooseAllPlatforms = true
	if err := (BucketLoader{Redis: rdb, Chooser: server.Chooser}).RefreshAll(ctx); err != nil {
		t.Fatal(err)
	}
	auth := NewAuthenticator(rdb)
	if err := auth.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	router := NewRouter(server, auth)

	// Body-supplied tenant/group must be ignored; only the token decides.
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tenant_id":"tenant-b","group":"b"}`)
	call := func(token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder
	}
	if rec := call(""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call("gwt_unknown"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call("gwt_a"); rec.Code != http.StatusOK {
		t.Fatalf("tenant a: %d %s", rec.Code, rec.Body.String())
	}
	release := latestRelease(t, rdb)
	if release.TenantID != "tenant-a" || release.AccountID != "acc-a" || release.Provider != "grok" {
		t.Fatalf("release did not follow the token's tenant/group: %+v", release)
	}
	if rec := call("gwt_b"); rec.Code != http.StatusOK {
		t.Fatalf("tenant b: %d %s", rec.Code, rec.Body.String())
	}
	release = latestRelease(t, rdb)
	if release.TenantID != "tenant-b" || release.AccountID != "acc-b" {
		t.Fatalf("tenant b release: %+v", release)
	}
	// metrics stays unauthenticated
	metrics := httptest.NewRecorder()
	router.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metrics.Code != http.StatusOK {
		t.Fatalf("metrics: %d", metrics.Code)
	}
	var decoded map[string]any
	if err := json.Unmarshal(call("gwt_a").Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
}
