package claude

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

func TestQuotaFixtureUsesClaudeOAuthHeadersAndNormalizes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/quota" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer claude-access" || r.Header.Get("anthropic-beta") != "oauth-2025-04-20" || r.Header.Get("x-api-key") != "" {
			t.Fatalf("auth headers = %#v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[{"scope":"account","unit":"credit","limit":40,"remaining":12}]}`)
	}))
	defer server.Close()
	p := NewConsoleOAuth(nil)
	p.Config.APIBaseURL = server.URL
	p.Config.QuotaPath = "/quota"
	got, err := p.Quota(context.Background(), contracts.Credential{AccessToken: "claude-access"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].Unit != "credit" || *got.Items[0].Remaining != 12 {
		t.Fatalf("quota = %#v", got)
	}
}

func TestQuotaFixtureUsesClaudeAPIKeyHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "claude-key" || r.Header.Get("Authorization") != "" {
			t.Fatalf("auth headers = %#v", r.Header)
		}
		_, _ = io.WriteString(w, `{"items":[]}`)
	}))
	defer server.Close()
	p := NewAPIKey(nil)
	p.Config.APIBaseURL = server.URL
	p.Config.QuotaPath = "/quota"
	got, err := p.Quota(context.Background(), contracts.Credential{AccessToken: "claude-key"})
	if err != nil || len(got.Items) != 0 {
		t.Fatalf("quota = %#v err=%v", got, err)
	}
}
