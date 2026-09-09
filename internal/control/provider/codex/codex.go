package codex

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

// Provider implements the Codex OAuth/API-key control-plane behavior. It does
// not implement the Web chat-requirements challenge flow.
type Provider struct {
	HTTP   providerapi.HTTPClient
	Config providerapi.EndpointProfile
}

func NewOAuth(client *http.Client) *Provider {
	return &Provider{HTTP: providerapi.HTTPClient{Client: client}, Config: providerapi.CodexChatGPTProfile()}
}

func NewAPIKey(client *http.Client) *Provider {
	return &Provider{HTTP: providerapi.HTTPClient{Client: client}, Config: providerapi.CodexAPIKeyProfile()}
}

func (p *Provider) Kind() string { return providerapi.KindCodex }

func (p *Provider) AuthMode() string { return p.Config.AuthMode }

func (p *Provider) Authorize(ctx context.Context, req contracts.ImportRequest) (contracts.TokenBundle, error) {
	if req.Provider != "" && req.Provider != p.Kind() {
		return contracts.TokenBundle{}, fmt.Errorf("%w: provider mismatch", contracts.ErrInvalidContract)
	}
	if err := providerapi.ValidateImportAuthMode(p.Config, req.AuthMode); err != nil {
		return contracts.TokenBundle{}, err
	}
	if req.TokenBundle != nil {
		if strings.TrimSpace(req.TokenBundle.AccessToken) == "" && strings.TrimSpace(req.TokenBundle.RefreshToken) == "" {
			return contracts.TokenBundle{}, fmt.Errorf("%w: imported token bundle is empty", contracts.ErrInvalidContract)
		}
		return *req.TokenBundle, nil
	}
	if req.StaticKey != "" {
		if p.Config.AuthMode != providerapi.AuthModeAPIKey {
			return contracts.TokenBundle{}, fmt.Errorf("%w: static key with OAuth profile", contracts.ErrInvalidContract)
		}
		return contracts.TokenBundle{AccessToken: req.StaticKey}, nil
	}
	code, verifier, redirectURI := req.Metadata["code"], req.Metadata["code_verifier"], req.Metadata["redirect_uri"]
	return p.HTTP.ExchangeCodeWithProxy(ctx, p.Config, code, verifier, redirectURI, req.Metadata["state"], req.Metadata["proxy"])
}

// Refresh preserves the legacy Provider method. OAuth callers must use
// RefreshOAuth so the refresh token stays in the control plane.
func (p *Provider) Refresh(_ context.Context, credential contracts.Credential) (contracts.TokenBundle, error) {
	if p.Config.AuthMode == providerapi.AuthModeAPIKey {
		return contracts.TokenBundle{AccessToken: credential.AccessToken, ExpiresAt: credential.ExpiresAt}, nil
	}
	return contracts.TokenBundle{}, providerapi.ErrOAuthRefreshUnsupported
}

func (p *Provider) RefreshOAuth(ctx context.Context, current contracts.TokenBundle) (contracts.TokenBundle, error) {
	return p.HTTP.Refresh(ctx, p.Config, current)
}

func (p *Provider) Revoke(ctx context.Context, credential contracts.Credential) error {
	return p.HTTP.RevokeWithProxy(ctx, p.Config, credential.AccessToken, "access_token", credential.Proxy)
}

func (p *Provider) RevokeOAuth(ctx context.Context, current contracts.TokenBundle) error {
	token := current.RefreshToken
	tokenType := "refresh_token"
	if token == "" {
		token = current.AccessToken
		tokenType = "access_token"
	}
	return p.HTTP.RevokeWithProxy(ctx, p.Config, token, tokenType, current.Metadata["proxy"])
}

func (p *Provider) Profile(contracts.Account) contracts.UpstreamProfile {
	return contracts.UpstreamProfile{BaseURL: p.Config.APIBaseURL, Protocol: "openai_responses", InferencePath: p.Config.InferencePath, TLSFingerprint: p.Config.TLSFingerprint}
}
