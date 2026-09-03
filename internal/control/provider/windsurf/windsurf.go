package windsurf

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

type Provider struct {
	HTTP   providerapi.HTTPClient
	Config providerapi.EndpointProfile
}

func NewAPIKey(client *http.Client) *Provider {
	return &Provider{HTTP: providerapi.HTTPClient{Client: client}, Config: providerapi.WindsurfAPIKeyProfile()}
}

func (p *Provider) Kind() string { return providerapi.KindWindsurf }

func (p *Provider) AuthMode() string { return p.Config.AuthMode }

func (p *Provider) Authorize(_ context.Context, req contracts.ImportRequest) (contracts.TokenBundle, error) {
	if req.Provider != "" && req.Provider != p.Kind() {
		return contracts.TokenBundle{}, fmt.Errorf("%w: provider mismatch", contracts.ErrInvalidContract)
	}
	if err := providerapi.ValidateImportAuthMode(p.Config, req.AuthMode); err != nil {
		return contracts.TokenBundle{}, err
	}
	if strings.TrimSpace(req.StaticKey) == "" {
		return contracts.TokenBundle{}, fmt.Errorf("%w: Windsurf API key is required", contracts.ErrInvalidContract)
	}
	return contracts.TokenBundle{AccessToken: req.StaticKey}, nil
}

func (p *Provider) Refresh(_ context.Context, credential contracts.Credential) (contracts.TokenBundle, error) {
	if strings.TrimSpace(credential.AccessToken) == "" {
		return contracts.TokenBundle{}, fmt.Errorf("%w: Windsurf API key is required", contracts.ErrInvalidContract)
	}
	return contracts.TokenBundle{AccessToken: credential.AccessToken, ExpiresAt: credential.ExpiresAt}, nil
}

func (p *Provider) Revoke(context.Context, contracts.Credential) error {
	return contracts.ErrUnsupportedCapability
}

func (p *Provider) Profile(contracts.Account) contracts.UpstreamProfile {
	return contracts.UpstreamProfile{BaseURL: p.Config.APIBaseURL}
}

func (p *Provider) ProfileForImport(account contracts.Account, req contracts.ImportRequest) (contracts.UpstreamProfile, error) {
	path := strings.TrimSpace(req.Metadata["inference_path"])
	protocol := strings.TrimSpace(req.Metadata["protocol"])
	if protocol != "openai_chat" {
		return contracts.UpstreamProfile{}, fmt.Errorf("%w: Windsurf protocol must explicitly be openai_chat", contracts.ErrInvalidContract)
	}
	parsed, err := url.Parse(path)
	if err != nil || path == "" || len(path) > 2048 || !strings.HasPrefix(path, "/") || parsed.IsAbs() || parsed.Host != "" {
		return contracts.UpstreamProfile{}, fmt.Errorf("%w: Windsurf inference_path must be an absolute URL path", contracts.ErrInvalidContract)
	}
	for _, key := range []string{"ide_name", "ide_version", "extension_name", "extension_version"} {
		if value := req.Metadata[key]; len(value) > 128 || strings.ContainsAny(value, "\r\n") {
			return contracts.UpstreamProfile{}, fmt.Errorf("%w: Windsurf %s is invalid", contracts.ErrInvalidContract, key)
		}
	}
	ideVersion := metadataOrDefault(req.Metadata, "ide_version", "1.20.9")
	extensionVersion := metadataOrDefault(req.Metadata, "extension_version", ideVersion)
	profile := p.Profile(account)
	profile.Protocol = protocol
	profile.InferencePath = path
	profile.UserAgent = "Windsurf/" + ideVersion + " Codeium/" + extensionVersion
	profile.ExtraHeaders = map[string]string{
		"X-Codeium-IDE-Name":          metadataOrDefault(req.Metadata, "ide_name", "windsurf"),
		"X-Codeium-IDE-Version":       ideVersion,
		"X-Codeium-Extension-Name":    metadataOrDefault(req.Metadata, "extension_name", "windsurf"),
		"X-Codeium-Extension-Version": extensionVersion,
	}
	return profile, nil
}

func (p *Provider) Quota(context.Context, contracts.Credential) (contracts.QuotaInfo, error) {
	return contracts.QuotaInfo{}, nil
}

func metadataOrDefault(metadata map[string]string, key, fallback string) string {
	value := strings.TrimSpace(metadata[key])
	if value == "" {
		return fallback
	}
	return value
}
