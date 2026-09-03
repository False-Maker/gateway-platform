package grok

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

func TestAPIKeyProviderProfileLifecycleAndInference(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer grok-key" || r.Header.Get("x-api-key") != "" {
			t.Fatalf("request path=%q headers=%#v", r.URL.Path, r.Header)
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`)
	}))
	defer server.Close()

	p := NewAPIKey(server.Client())
	p.Config.APIBaseURL = server.URL
	bundle, err := p.Authorize(context.Background(), contracts.ImportRequest{Provider: providerapi.KindGrok, StaticKey: "grok-key"})
	if err != nil || bundle.AccessToken != "grok-key" {
		t.Fatalf("authorize bundle=%#v err=%v", bundle, err)
	}
	refreshed, err := p.Refresh(context.Background(), contracts.Credential{AccessToken: bundle.AccessToken})
	if err != nil || refreshed.AccessToken != "grok-key" {
		t.Fatalf("refresh bundle=%#v err=%v", refreshed, err)
	}
	profile := p.Profile(contracts.Account{})
	if profile.Protocol != "openai_chat" || profile.InferencePath != "/v1/chat/completions" {
		t.Fatalf("profile=%#v", profile)
	}
	response, err := p.HTTP.Inference(context.Background(), p.Config, bundle.AccessToken, []byte(`{"model":"grok","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil || len(response) == 0 {
		t.Fatalf("response=%q err=%v", response, err)
	}
	if err := p.Revoke(context.Background(), contracts.Credential{AccessToken: bundle.AccessToken}); !errors.Is(err, contracts.ErrUnsupportedCapability) {
		t.Fatalf("revoke error=%v", err)
	}
}

func TestAPIKeyProviderRejectsMissingOrWrongProvider(t *testing.T) {
	p := NewAPIKey(nil)
	for _, req := range []contracts.ImportRequest{{Provider: providerapi.KindGrok}, {Provider: providerapi.KindCodex, StaticKey: "key"}, {Provider: providerapi.KindGrok, AuthMode: providerapi.AuthModeOAuth, StaticKey: "key"}} {
		if _, err := p.Authorize(context.Background(), req); !errors.Is(err, contracts.ErrInvalidContract) {
			t.Fatalf("request=%#v error=%v", req, err)
		}
	}
}

func TestGrokQuotaIsExplicitlyMissing(t *testing.T) {
	quota, err := NewAPIKey(nil).Quota(context.Background(), contracts.Credential{AccessToken: "key"})
	if err != nil || quota.Items != nil {
		t.Fatalf("quota=%#v err=%v", quota, err)
	}
}
