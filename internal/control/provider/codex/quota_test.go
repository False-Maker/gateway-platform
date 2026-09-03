package codex

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

func TestQuotaFixtureUsesCodexBearerAuthAndNormalizes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/quota" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer codex-access" || r.Header.Get("x-api-key") != "" {
			t.Fatalf("auth headers = %#v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[{"scope":"account","unit":"token","limit":1000,"remaining":750},{"scope":"model","model":"gpt-5","unit":"request","limit":100,"remaining":90}]}`)
	}))
	defer server.Close()
	p := NewOAuth(nil)
	p.Config.APIBaseURL = server.URL
	p.Config.QuotaPath = "/quota"
	got, err := p.Quota(context.Background(), contracts.Credential{AccessToken: "codex-access"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 2 || got.Items[0].Scope != "account" || got.Items[1].Model != "gpt-5" || *got.Items[1].Remaining != 90 {
		t.Fatalf("quota = %#v", got)
	}
}

func TestQuotaWithoutConfiguredEndpointIsMissing(t *testing.T) {
	p := NewOAuth(nil)
	got, err := p.Quota(context.Background(), contracts.Credential{})
	if err != nil || len(got.Items) != 0 {
		t.Fatalf("quota = %#v err=%v", got, err)
	}
}
