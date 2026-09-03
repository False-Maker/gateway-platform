package builtin

import (
	"testing"

	"github.com/elucid/gateway-platform/internal/control/provider"
)

func TestRegisterAllInstallsLocallyVerifiedProviders(t *testing.T) {
	RegisterAll(nil)
	for _, kind := range []string{provider.KindCodex, provider.KindClaude, provider.KindGemini, provider.KindGrok} {
		if registered, ok := provider.Get(kind); !ok || registered.Kind() != kind {
			t.Fatalf("provider %q was not registered", kind)
		}
	}
	for _, tc := range []struct {
		kind string
		mode string
	}{
		{kind: provider.KindCodex, mode: provider.AuthModeOAuth},
		{kind: provider.KindCodex, mode: provider.AuthModeAPIKey},
		{kind: provider.KindClaude, mode: provider.AuthModeOAuth},
		{kind: provider.KindClaude, mode: provider.AuthModeAPIKey},
		{kind: provider.KindGemini, mode: provider.AuthModeAPIKey},
		{kind: provider.KindGrok, mode: provider.AuthModeAPIKey},
	} {
		registered, ok := provider.GetForAuth(tc.kind, tc.mode)
		modeProvider, typed := registered.(provider.AuthModeProvider)
		if !ok || !typed || modeProvider.AuthMode() != tc.mode {
			t.Fatalf("provider %q auth mode %q was not registered: %#v", tc.kind, tc.mode, registered)
		}
	}
}
