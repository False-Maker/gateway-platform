package windsurf

import (
	"context"
	"errors"
	"testing"

	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

func TestProviderRequiresExplicitCompatibleEndpoint(t *testing.T) {
	p := NewAPIKey(nil)
	req := contracts.ImportRequest{Provider: providerapi.KindWindsurf, AuthMode: providerapi.AuthModeAPIKey, StaticKey: "codeium-key", Metadata: map[string]string{"protocol": "openai_chat", "inference_path": "/configured/chat", "ide_version": "1.2.3", "extension_version": "1.2.4"}}
	bundle, err := p.Authorize(context.Background(), req)
	if err != nil || bundle.AccessToken != "codeium-key" {
		t.Fatalf("bundle=%#v err=%v", bundle, err)
	}
	profile, err := p.ProfileForImport(contracts.Account{}, req)
	if err != nil {
		t.Fatal(err)
	}
	if profile.BaseURL != "https://server.codeium.com" || profile.InferencePath != "/configured/chat" || profile.Protocol != "openai_chat" || profile.UserAgent != "Windsurf/1.2.3 Codeium/1.2.4" {
		t.Fatalf("profile=%#v", profile)
	}
}

func TestProviderRejectsImplicitOrRemoteEndpoint(t *testing.T) {
	p := NewAPIKey(nil)
	for _, metadata := range []map[string]string{
		{"inference_path": "/configured/chat"},
		{"protocol": "openai_chat"},
		{"protocol": "openai_chat", "inference_path": "https://example.test/chat"},
		{"protocol": "gemini_generate", "inference_path": "/configured/chat"},
	} {
		if _, err := p.ProfileForImport(contracts.Account{}, contracts.ImportRequest{Metadata: metadata}); !errors.Is(err, contracts.ErrInvalidContract) {
			t.Fatalf("metadata=%#v error=%v", metadata, err)
		}
	}
}
