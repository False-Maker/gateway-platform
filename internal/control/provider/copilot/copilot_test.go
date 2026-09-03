package copilot

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

func TestProviderImportsBundleAndRefreshesCopilotToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/copilot_internal/v2/token" {
			t.Fatalf("method=%s path=%s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "token github-token" || r.Header.Get("editor-version") != "vscode/1.109.3" {
			t.Fatalf("headers=%#v", r.Header)
		}
		_, _ = io.WriteString(w, `{"token":"copilot-new","expires_at":1770000000,"refresh_in":1200}`)
	}))
	defer server.Close()

	p := NewOAuth(server.Client())
	p.Config.OAuthTokenURL = server.URL + "/copilot_internal/v2/token"
	bundle, err := p.Authorize(context.Background(), contracts.ImportRequest{
		Provider: providerapi.KindCopilot,
		AuthMode: providerapi.AuthModeOAuth,
		TokenBundle: &contracts.TokenBundle{
			AccessToken:  "copilot-old",
			RefreshToken: "github-token",
			AccountID:    "octocat",
		},
	})
	if err != nil || bundle.AccessToken != "copilot-old" {
		t.Fatalf("bundle=%#v err=%v", bundle, err)
	}
	refreshed, err := p.RefreshOAuth(context.Background(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.AccessToken != "copilot-new" || refreshed.RefreshToken != "github-token" || refreshed.AccountID != "octocat" {
		t.Fatalf("refreshed=%#v", refreshed)
	}
	if want := time.Unix(1770000000, 0).UTC(); !refreshed.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at=%s want=%s", refreshed.ExpiresAt, want)
	}
	profile := p.Profile(contracts.Account{})
	if profile.BaseURL != "https://api.githubcopilot.com" || profile.InferencePath != "/chat/completions" || profile.Protocol != "openai_chat" {
		t.Fatalf("profile=%#v", profile)
	}
}

func TestProviderRejectsIncompleteOrMismatchedImport(t *testing.T) {
	p := NewOAuth(nil)
	for _, req := range []contracts.ImportRequest{
		{Provider: providerapi.KindCopilot, AuthMode: providerapi.AuthModeOAuth},
		{Provider: providerapi.KindCopilot, AuthMode: providerapi.AuthModeAPIKey, TokenBundle: &contracts.TokenBundle{AccessToken: "token"}},
		{Provider: providerapi.KindClaude, AuthMode: providerapi.AuthModeOAuth, TokenBundle: &contracts.TokenBundle{AccessToken: "token"}},
	} {
		if _, err := p.Authorize(context.Background(), req); !errors.Is(err, contracts.ErrInvalidContract) {
			t.Fatalf("request=%#v error=%v", req, err)
		}
	}
	if _, err := p.RefreshOAuth(context.Background(), contracts.TokenBundle{AccessToken: "short"}); !errors.Is(err, contracts.ErrInvalidContract) {
		t.Fatalf("refresh error=%v", err)
	}
}

func TestProviderBoundsRefreshResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, maxTokenResponseBytes+1))
	}))
	defer server.Close()
	p := NewOAuth(server.Client())
	p.Config.OAuthTokenURL = server.URL
	if _, err := p.RefreshOAuth(context.Background(), contracts.TokenBundle{RefreshToken: "github-token"}); err == nil {
		t.Fatal("oversized response was accepted")
	}
}

func TestInferenceHeadersIncludeCopilotRequestIdentity(t *testing.T) {
	headers, err := providerapi.GitHubCopilotProfile().InferenceHeaders("copilot-token")
	if err != nil {
		t.Fatal(err)
	}
	requestID := headers.Get("x-request-id")
	if requestID == "" || headers.Get("x-interaction-id") != requestID || headers.Get("x-agent-task-id") != requestID {
		t.Fatalf("request identity headers=%#v", headers)
	}
	if headers.Get("X-Initiator") != "user" || headers.Get("copilot-integration-id") != "vscode-chat" {
		t.Fatalf("Copilot headers=%#v", headers)
	}
}

func TestProviderQuotaIsExplicitlyMissing(t *testing.T) {
	quota, err := NewOAuth(nil).Quota(context.Background(), contracts.Credential{AccessToken: "token"})
	if err != nil || quota.Items != nil {
		t.Fatalf("quota=%#v err=%v", quota, err)
	}
}
