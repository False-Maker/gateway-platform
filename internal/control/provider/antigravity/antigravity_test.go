package antigravity

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

func TestProviderImportsTokenAndBuildsProjectProfile(t *testing.T) {
	p := NewOAuth(nil)
	req := contracts.ImportRequest{
		Provider:    providerapi.KindAntigravity,
		AuthMode:    providerapi.AuthModeOAuth,
		TokenBundle: &contracts.TokenBundle{AccessToken: "access", RefreshToken: "refresh"},
		Metadata:    map[string]string{"project_id": "cloud-project-1"},
	}
	bundle, err := p.Authorize(context.Background(), req)
	if err != nil || bundle.AccessToken != "access" {
		t.Fatalf("bundle=%#v err=%v", bundle, err)
	}
	profile, err := p.ProfileForImport(contracts.Account{}, req)
	if err != nil {
		t.Fatal(err)
	}
	if profile.BaseURL != "https://cloudcode-pa.googleapis.com" || profile.InferencePath != "/v1internal:generateContent" || profile.Protocol != "gemini_generate" {
		t.Fatalf("profile=%#v", profile)
	}
	if profile.ExtraHeaders["X-Goog-User-Project"] != "cloud-project-1" || profile.UserAgent != defaultUserAgent {
		t.Fatalf("profile metadata=%#v", profile)
	}
}

func TestProviderRejectsMissingProjectAndToken(t *testing.T) {
	p := NewOAuth(nil)
	if _, err := p.Authorize(context.Background(), contracts.ImportRequest{Provider: providerapi.KindAntigravity, AuthMode: providerapi.AuthModeOAuth}); !errors.Is(err, contracts.ErrInvalidContract) {
		t.Fatalf("authorize error=%v", err)
	}
	if _, err := p.ProfileForImport(contracts.Account{}, contracts.ImportRequest{}); !errors.Is(err, contracts.ErrInvalidContract) {
		t.Fatalf("profile error=%v", err)
	}
}

func TestProviderQuotaIsExplicitlyMissing(t *testing.T) {
	quota, err := NewOAuth(nil).Quota(context.Background(), contracts.Credential{AccessToken: "token"})
	if err != nil || quota.Items != nil {
		t.Fatalf("quota=%#v err=%v", quota, err)
	}
}

func TestProviderRefreshesGoogleTokenWithFormEncoding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		values, err := url.ParseQuery(string(body))
		if err != nil || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || values.Get("grant_type") != "refresh_token" || values.Get("refresh_token") != "refresh-old" || values.Get("client_id") == "" || values.Get("client_secret") == "" {
			t.Fatalf("headers=%#v body=%q err=%v", r.Header, body, err)
		}
		_, _ = io.WriteString(w, `{"access_token":"access-new","expires_in":3600,"scope":"scope-a scope-b"}`)
	}))
	defer server.Close()
	p := NewOAuth(server.Client())
	p.Config.OAuthTokenURL = server.URL
	refreshed, err := p.RefreshOAuth(context.Background(), contracts.TokenBundle{AccessToken: "access-old", RefreshToken: "refresh-old", Metadata: map[string]string{"client_secret": "fixture-secret"}})
	if err != nil || refreshed.AccessToken != "access-new" || refreshed.RefreshToken != "refresh-old" || refreshed.ExpiresAt.IsZero() || len(refreshed.Scopes) != 2 {
		t.Fatalf("bundle=%#v err=%v", refreshed, err)
	}
}
