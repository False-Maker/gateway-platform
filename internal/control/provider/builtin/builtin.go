package builtin

import (
	"net/http"

	"github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/internal/control/provider/antigravity"
	"github.com/elucid/gateway-platform/internal/control/provider/claude"
	"github.com/elucid/gateway-platform/internal/control/provider/codex"
	"github.com/elucid/gateway-platform/internal/control/provider/copilot"
	"github.com/elucid/gateway-platform/internal/control/provider/gemini"
	"github.com/elucid/gateway-platform/internal/control/provider/grok"
	"github.com/elucid/gateway-platform/internal/control/provider/kiro"
	"github.com/elucid/gateway-platform/internal/control/provider/windsurf"
)

// RegisterAll installs the locally verified providers in the control-plane registry. The
// default HTTP client is used only when a caller does not inject a fixture.
func RegisterAll(client *http.Client) {
	provider.Register(antigravity.NewOAuth(client))
	provider.Register(codex.NewOAuth(client))
	provider.Register(codex.NewAPIKey(client))
	provider.Register(claude.NewConsoleOAuth(client))
	provider.Register(claude.NewAPIKey(client))
	provider.Register(copilot.NewOAuth(client))
	provider.Register(gemini.NewAPIKey(client))
	provider.Register(grok.NewAPIKey(client))
	provider.Register(kiro.NewOAuth(client))
	provider.Register(windsurf.NewAPIKey(client))
}
