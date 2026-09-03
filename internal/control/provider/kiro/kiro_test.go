package kiro

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

func TestProviderImportsProfileAndRefreshesDesktopToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&payload) != nil || payload["refreshToken"] != "refresh-old" || r.Header.Get("User-Agent") != "KiroIDE-0.12.155-machine-1" {
			t.Fatalf("request method=%s payload=%#v", r.Method, payload)
		}
		_, _ = io.WriteString(w, `{"accessToken":"access-new","refreshToken":"refresh-new","expiresIn":3600}`)
	}))
	defer server.Close()
	p := NewOAuth(server.Client())
	p.Config.OAuthTokenURL = server.URL
	req := contracts.ImportRequest{Provider: providerapi.KindKiro, AuthMode: providerapi.AuthModeOAuth, TokenBundle: &contracts.TokenBundle{AccessToken: "access-old", RefreshToken: "refresh-old"}, Metadata: map[string]string{"profile_arn": "arn:aws:codewhisperer:us-east-1:123456789012:profile/test", "machine_id": "machine-1"}}
	bundle, err := p.Authorize(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Metadata["profile_arn"] != req.Metadata["profile_arn"] || bundle.Metadata["machine_id"] != req.Metadata["machine_id"] {
		t.Fatalf("bundle metadata=%#v", bundle.Metadata)
	}
	profile, err := p.ProfileForImport(contracts.Account{ID: "account-1"}, req)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Metadata["profile_arn"] != req.Metadata["profile_arn"] || profile.ExtraHeaders["X-Amz-Target"] == "" || profile.InferencePath != "/generateAssistantResponse" {
		t.Fatalf("profile=%#v", profile)
	}
	refreshed, err := p.RefreshOAuth(context.Background(), bundle)
	if err != nil || refreshed.AccessToken != "access-new" || refreshed.RefreshToken != "refresh-new" || refreshed.ExpiresAt.IsZero() {
		t.Fatalf("bundle=%#v err=%v", refreshed, err)
	}
}

func TestProviderRejectsMissingTokenOrProfileARN(t *testing.T) {
	p := NewOAuth(nil)
	if _, err := p.Authorize(context.Background(), contracts.ImportRequest{Provider: providerapi.KindKiro, AuthMode: providerapi.AuthModeOAuth}); !errors.Is(err, contracts.ErrInvalidContract) {
		t.Fatalf("authorize error=%v", err)
	}
	if _, err := p.ProfileForImport(contracts.Account{ID: "account-1"}, contracts.ImportRequest{}); !errors.Is(err, contracts.ErrInvalidContract) {
		t.Fatalf("profile error=%v", err)
	}
}
