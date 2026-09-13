package builtin

import (
	"testing"

	"github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/pkg/contracts"
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

// Overview §6.2 requires every registered provider to state a usage_integrity.
// The Provider interface already makes omitting it a compile error; this asserts
// the value is inside the contract and that the first batch matches §6.2's
// table, which is the part a compiler cannot check.
func TestEveryRegisteredProviderDeclaresUsageIntegrity(t *testing.T) {
	RegisterAll(nil)
	kinds := []string{
		provider.KindAntigravity, provider.KindClaude, provider.KindCodex,
		provider.KindCopilot, provider.KindGemini, provider.KindGrok,
		provider.KindKiro, provider.KindWindsurf,
	}
	for _, kind := range kinds {
		policy, ok := provider.UsageIntegrityFor(kind)
		if !ok {
			t.Fatalf("provider %q declares no usable usage_integrity", kind)
		}
		// §6.2's first-batch table is failover on every row, and the section
		// gates `zero` behind a review and `estimated` behind a tokenizer
		// calibration report. A change here is a design decision, not a tweak.
		if policy != contracts.UsageIntegrityFailover {
			t.Fatalf("provider %q declares %q; §6.2's first batch is failover on every row", kind, policy)
		}
	}
}

func TestUsageIntegrityForRejectsUnregisteredProviders(t *testing.T) {
	RegisterAll(nil)
	if _, ok := provider.UsageIntegrityFor("provider-that-was-never-registered"); ok {
		t.Fatal("an unregistered provider resolved a usage_integrity policy")
	}
}
