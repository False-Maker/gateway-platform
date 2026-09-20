package gateway

import (
	"testing"

	"github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/internal/control/provider/builtin"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

// A17: overview §5 makes this the one deep coupling between the two roles --
// "control 写下名字, gateway 必须真的实现该 utls profile". Nothing enforced it.
//
// The failure it prevents is quiet: a new provider naming a fingerprint that
// gateway does not implement builds, tests and ships green, and is only found
// at runtime when every request on that channel fails closed with 503. The
// channel is dead but nothing says so at the time the mistake is made.
//
// Driven off the registry rather than a list of names on purpose. A pin that
// has to be updated by hand is not an invariant -- the same lesson F3 taught
// when A16's hand-written metric table let the very next commit through.
func TestEveryProviderFingerprintHasAGatewayProfile(t *testing.T) {
	builtin.RegisterAll(nil)
	providers := provider.RegisteredProviders()
	if len(providers) == 0 {
		t.Fatal("no providers registered; this test would pass vacuously")
	}
	checked := 0
	for _, p := range providers {
		profile := p.Profile(contracts.Account{ID: "a17-probe", Provider: p.Kind()})
		if profile.TLSFingerprint == "" {
			// No fingerprint means "use the default transport", which is a
			// legitimate choice for plain apikey channels.
			continue
		}
		checked++
		if _, err := lookupTLSProfile(profile.TLSFingerprint); err != nil {
			t.Errorf("provider %s writes TLSFingerprint %q, which gateway does not implement: %v",
				p.Kind(), profile.TLSFingerprint, err)
		}
	}
	if checked == 0 {
		t.Fatal("no provider produced a TLSFingerprint; the probe is not exercising the invariant")
	}
	t.Logf("checked %d provider fingerprints against the gateway registry", checked)
}
