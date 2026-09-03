package grok

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

type Provider struct {
	HTTP   providerapi.HTTPClient
	Config providerapi.EndpointProfile
}

func NewAPIKey(client *http.Client) *Provider {
	return &Provider{HTTP: providerapi.HTTPClient{Client: client}, Config: providerapi.GrokAPIKeyProfile()}
}

func (p *Provider) Kind() string { return providerapi.KindGrok }

func (p *Provider) AuthMode() string { return p.Config.AuthMode }

func (p *Provider) Authorize(_ context.Context, req contracts.ImportRequest) (contracts.TokenBundle, error) {
	if req.Provider != "" && req.Provider != p.Kind() {
		return contracts.TokenBundle{}, fmt.Errorf("%w: provider mismatch", contracts.ErrInvalidContract)
	}
	if err := providerapi.ValidateImportAuthMode(p.Config, req.AuthMode); err != nil {
		return contracts.TokenBundle{}, err
	}
	if strings.TrimSpace(req.StaticKey) == "" {
		return contracts.TokenBundle{}, fmt.Errorf("%w: Grok API key is required", contracts.ErrInvalidContract)
	}
	return contracts.TokenBundle{AccessToken: req.StaticKey}, nil
}

func (p *Provider) Refresh(_ context.Context, credential contracts.Credential) (contracts.TokenBundle, error) {
	if strings.TrimSpace(credential.AccessToken) == "" {
		return contracts.TokenBundle{}, fmt.Errorf("%w: Grok API key is required", contracts.ErrInvalidContract)
	}
	return contracts.TokenBundle{AccessToken: credential.AccessToken, ExpiresAt: credential.ExpiresAt}, nil
}

func (p *Provider) Revoke(context.Context, contracts.Credential) error {
	return contracts.ErrUnsupportedCapability
}

func (p *Provider) Profile(contracts.Account) contracts.UpstreamProfile {
	return contracts.UpstreamProfile{BaseURL: p.Config.APIBaseURL, Protocol: "openai_chat", InferencePath: p.Config.InferencePath}
}

func (p *Provider) Quota(context.Context, contracts.Credential) (contracts.QuotaInfo, error) {
	// xAI quota is observed from inference rate-limit headers; no verified
	// account-level quota endpoint is configured for the API-key profile.
	return contracts.QuotaInfo{}, nil
}
