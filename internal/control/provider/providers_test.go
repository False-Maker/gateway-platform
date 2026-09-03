package provider_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/internal/control/provider/antigravity"
	"github.com/elucid/gateway-platform/internal/control/provider/claude"
	"github.com/elucid/gateway-platform/internal/control/provider/codex"
	"github.com/elucid/gateway-platform/internal/control/provider/copilot"
	"github.com/elucid/gateway-platform/internal/control/provider/gemini"
	"github.com/elucid/gateway-platform/internal/control/provider/grok"
	"github.com/elucid/gateway-platform/internal/control/provider/kiro"
	"github.com/elucid/gateway-platform/internal/control/provider/windsurf"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

func TestConcreteProvidersImplementControlPlaneRefresh(t *testing.T) {
	var _ providerapi.Provider = (*antigravity.Provider)(nil)
	var _ providerapi.ImportProfileProvider = (*antigravity.Provider)(nil)
	var _ providerapi.OAuthRefresher = (*antigravity.Provider)(nil)
	var _ providerapi.Provider = (*codex.Provider)(nil)
	var _ providerapi.OAuthRefresher = (*codex.Provider)(nil)
	var _ providerapi.Provider = (*claude.Provider)(nil)
	var _ providerapi.OAuthRefresher = (*claude.Provider)(nil)
	var _ providerapi.Provider = (*copilot.Provider)(nil)
	var _ providerapi.OAuthRefresher = (*copilot.Provider)(nil)
	var _ providerapi.Provider = (*gemini.Provider)(nil)
	var _ providerapi.Provider = (*grok.Provider)(nil)
	var _ providerapi.Provider = (*kiro.Provider)(nil)
	var _ providerapi.OAuthRefresher = (*kiro.Provider)(nil)
	var _ providerapi.ImportProfileProvider = (*kiro.Provider)(nil)
	var _ providerapi.Provider = (*windsurf.Provider)(nil)
	var _ providerapi.ImportProfileProvider = (*windsurf.Provider)(nil)
}

func TestCodexAndClaudeRejectMismatchedImportAuthMode(t *testing.T) {
	for _, test := range []struct {
		name string
		p    providerapi.Provider
		req  contracts.ImportRequest
	}{
		{name: "codex oauth profile with api key mode", p: codex.NewOAuth(nil), req: contracts.ImportRequest{Provider: providerapi.KindCodex, AuthMode: providerapi.AuthModeAPIKey, TokenBundle: &contracts.TokenBundle{AccessToken: "access"}}},
		{name: "codex api key profile with oauth mode", p: codex.NewAPIKey(nil), req: contracts.ImportRequest{Provider: providerapi.KindCodex, AuthMode: providerapi.AuthModeOAuth, StaticKey: "key"}},
		{name: "claude oauth profile with api key mode", p: claude.NewConsoleOAuth(nil), req: contracts.ImportRequest{Provider: providerapi.KindClaude, AuthMode: providerapi.AuthModeAPIKey, TokenBundle: &contracts.TokenBundle{AccessToken: "access"}}},
		{name: "claude api key profile with oauth mode", p: claude.NewAPIKey(nil), req: contracts.ImportRequest{Provider: providerapi.KindClaude, AuthMode: providerapi.AuthModeOAuth, StaticKey: "key"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.p.Authorize(context.Background(), test.req); !errors.Is(err, contracts.ErrInvalidContract) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestConcreteProvidersUseFixtureHTTPClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fixture-access","refresh_token":"fixture-refresh","expires_in":60}`))
	}))
	defer server.Close()
	for _, test := range []struct {
		name string
		p    providerapi.Provider
	}{
		{name: "codex", p: codex.NewOAuth(server.Client())},
		{name: "claude", p: claude.NewConsoleOAuth(server.Client())},
	} {
		t.Run(test.name, func(t *testing.T) {
			var profile providerapi.EndpointProfile
			switch test.name {
			case "codex":
				profile = codex.NewOAuth(server.Client()).Config
			case "claude":
				profile = claude.NewConsoleOAuth(server.Client()).Config
			}
			profile.OAuthTokenURL = server.URL + "/oauth/token"
			var refresher providerapi.OAuthRefresher
			switch p := test.p.(type) {
			case *codex.Provider:
				p.Config = profile
				refresher = p
			case *claude.Provider:
				p.Config = profile
				refresher = p
			}
			got, err := refresher.RefreshOAuth(context.Background(), contracts.TokenBundle{RefreshToken: "fixture-refresh"})
			if err != nil || got.AccessToken != "fixture-access" {
				t.Fatalf("bundle=%#v err=%v", got, err)
			}
		})
	}
}
