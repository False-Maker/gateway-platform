package gemini

import (
	"context"
	"errors"
	"testing"

	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

func TestAPIKeyProviderProfileAndLifecycle(t *testing.T) {
	p := NewAPIKey(nil)
	bundle, err := p.Authorize(context.Background(), contracts.ImportRequest{Provider: providerapi.KindGemini, StaticKey: "gemini-key"})
	if err != nil || bundle.AccessToken != "gemini-key" {
		t.Fatalf("authorize bundle=%#v err=%v", bundle, err)
	}
	refreshed, err := p.Refresh(context.Background(), contracts.Credential{AccessToken: bundle.AccessToken})
	if err != nil || refreshed.AccessToken != "gemini-key" {
		t.Fatalf("refresh bundle=%#v err=%v", refreshed, err)
	}
	profile := p.Profile(contracts.Account{})
	if profile.Protocol != "gemini_generate" || profile.InferencePath != "/v1beta/models/{model}:generateContent" {
		t.Fatalf("profile=%#v", profile)
	}
	if err := p.Revoke(context.Background(), contracts.Credential{AccessToken: bundle.AccessToken}); !errors.Is(err, contracts.ErrUnsupportedCapability) {
		t.Fatalf("revoke error=%v", err)
	}
}

func TestAPIKeyProviderRejectsMissingOrWrongProvider(t *testing.T) {
	p := NewAPIKey(nil)
	for _, req := range []contracts.ImportRequest{{Provider: providerapi.KindGemini}, {Provider: providerapi.KindCodex, StaticKey: "key"}, {Provider: providerapi.KindGemini, AuthMode: providerapi.AuthModeOAuth, StaticKey: "key"}} {
		if _, err := p.Authorize(context.Background(), req); !errors.Is(err, contracts.ErrInvalidContract) {
			t.Fatalf("request=%#v error=%v", req, err)
		}
	}
}

func TestGeminiQuotaIsExplicitlyMissing(t *testing.T) {
	quota, err := NewAPIKey(nil).Quota(context.Background(), contracts.Credential{AccessToken: "key"})
	if err != nil || quota.Items != nil {
		t.Fatalf("quota=%#v err=%v", quota, err)
	}
}
