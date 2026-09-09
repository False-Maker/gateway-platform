package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elucid/gateway-platform/internal/snapshot"
	"github.com/redis/go-redis/v9"
)

func TestAuthenticatorFailsClosedAndDerivesContextFromSnapshot(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	ctx := context.Background()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	auth := NewAuthenticator(rdb)
	auth.Now = func() time.Time { return now }

	if err := auth.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate("gwt_anything"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty snapshot must reject: %v", err)
	}
	if _, err := auth.Authenticate(""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty token must reject: %v", err)
	}

	valid, expired := "gwt_valid", "gwt_expired"
	if err := snapshot.PublishTokens(ctx, rdb, map[string]snapshot.TokenRecord{
		snapshot.HashToken(valid):   {TokenID: "t1", TenantID: "tenant-a", PrincipalID: "p1", Group: "vip"},
		snapshot.HashToken(expired): {TokenID: "t2", TenantID: "tenant-b", PrincipalID: "p2", Group: "default", ExpiresAt: now.Add(-time.Second)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := auth.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	authCtx, err := auth.Authenticate(valid)
	if err != nil || authCtx.TenantID != "tenant-a" || authCtx.PrincipalID != "p1" || authCtx.Group != "vip" || authCtx.TokenID != "t1" {
		t.Fatalf("valid token context=%#v err=%v", authCtx, err)
	}
	if _, err := auth.Authenticate(expired); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired token must reject: %v", err)
	}
	if _, err := auth.Authenticate(valid + "x"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("near-miss token must reject: %v", err)
	}

	// Revocation propagates after the next refresh.
	if err := snapshot.PublishTokens(ctx, rdb, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(valid); err != nil {
		t.Fatalf("token revoked before refresh: %v", err)
	}
	if err := auth.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(valid); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked token survived refresh: %v", err)
	}
}

func TestAuthenticateRequestAcceptsBearerAndXAPIKeyOnly(t *testing.T) {
	auth := NewAuthenticator(nil)
	auth.Replace(map[string]snapshot.TokenRecord{snapshot.HashToken("gwt_ok"): {TokenID: "t", TenantID: "tenant", PrincipalID: "p", Group: "default"}})

	bearer := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	bearer.Header.Set("Authorization", "Bearer gwt_ok")
	if ctx, err := auth.AuthenticateRequest(bearer); err != nil || ctx.TenantID != "tenant" {
		t.Fatalf("bearer: ctx=%#v err=%v", ctx, err)
	}
	apiKey := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	apiKey.Header.Set("x-api-key", "gwt_ok")
	if ctx, err := auth.AuthenticateRequest(apiKey); err != nil || ctx.TenantID != "tenant" {
		t.Fatalf("x-api-key: ctx=%#v err=%v", ctx, err)
	}
	basic := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	basic.Header.Set("Authorization", "Basic Z3d0X29r")
	if _, err := auth.AuthenticateRequest(basic); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("non-bearer scheme accepted: %v", err)
	}
	none := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if _, err := auth.AuthenticateRequest(none); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("missing header accepted: %v", err)
	}
}
