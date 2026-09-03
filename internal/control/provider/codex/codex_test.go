package codex

import (
	"testing"

	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

func TestProfileDeclaresResponsesProtocol(t *testing.T) {
	for _, provider := range []*Provider{NewOAuth(nil), NewAPIKey(nil)} {
		profile := provider.Profile(contracts.Account{})
		if profile.Protocol != "openai_responses" || profile.InferencePath != provider.Config.InferencePath || profile.BaseURL != provider.Config.APIBaseURL {
			t.Fatalf("profile=%#v config=%#v", profile, provider.Config)
		}
	}
	if providerapi.CodexChatGPTProfile().InferencePath != "/responses" {
		t.Fatal("Codex fixture profile path changed unexpectedly")
	}
}
