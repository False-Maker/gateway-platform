package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

const maxTokenResponseBytes = 1 << 20

type Provider struct {
	Client *http.Client
	Config providerapi.EndpointProfile
}

func NewOAuth(client *http.Client) *Provider {
	return &Provider{Client: client, Config: providerapi.GitHubCopilotProfile()}
}

func (p *Provider) Kind() string { return providerapi.KindCopilot }

func (p *Provider) AuthMode() string { return p.Config.AuthMode }

func (p *Provider) Authorize(_ context.Context, req contracts.ImportRequest) (contracts.TokenBundle, error) {
	if req.Provider != "" && req.Provider != p.Kind() {
		return contracts.TokenBundle{}, fmt.Errorf("%w: provider mismatch", contracts.ErrInvalidContract)
	}
	if err := providerapi.ValidateImportAuthMode(p.Config, req.AuthMode); err != nil {
		return contracts.TokenBundle{}, err
	}
	if req.TokenBundle == nil || strings.TrimSpace(req.TokenBundle.AccessToken) == "" {
		return contracts.TokenBundle{}, fmt.Errorf("%w: imported Copilot token bundle requires access_token", contracts.ErrInvalidContract)
	}
	return *req.TokenBundle, nil
}

func (p *Provider) Refresh(_ context.Context, _ contracts.Credential) (contracts.TokenBundle, error) {
	return contracts.TokenBundle{}, providerapi.ErrOAuthRefreshUnsupported
}

// RefreshOAuth exchanges the retained GitHub OAuth token for a short-lived
// Copilot API token. Interactive GitHub device authorization is deliberately
// left to the wrapper.
func (p *Provider) RefreshOAuth(ctx context.Context, current contracts.TokenBundle) (contracts.TokenBundle, error) {
	githubToken := strings.TrimSpace(current.RefreshToken)
	if githubToken == "" {
		return contracts.TokenBundle{}, fmt.Errorf("%w: GitHub OAuth token is missing", contracts.ErrInvalidContract)
	}
	if err := p.Config.Validate(); err != nil {
		return contracts.TokenBundle{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.Config.OAuthTokenURL, nil)
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "token "+githubToken)
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.44.0")
	req.Header.Set("editor-version", "vscode/1.109.3")
	req.Header.Set("editor-plugin-version", "copilot-chat/0.44.0")
	req.Header.Set("x-github-api-version", "2025-05-01")
	req.Header.Set("x-vscode-user-agent-library-version", "electron-fetch")
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponseBytes+1))
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	if len(body) > maxTokenResponseBytes {
		return contracts.TokenBundle{}, fmt.Errorf("Copilot token response exceeds %d bytes", maxTokenResponseBytes)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		class := contracts.ErrorForbiddenCapability
		if resp.StatusCode == http.StatusUnauthorized {
			class = contracts.ErrorAuthInvalid
		} else if resp.StatusCode == http.StatusTooManyRequests {
			class = contracts.ErrorRateLimitedKnown
		} else if resp.StatusCode >= http.StatusInternalServerError {
			class = contracts.ErrorUpstream5xx
		}
		return contracts.TokenBundle{}, &providerapi.HTTPError{Operation: "Copilot token", StatusCode: resp.StatusCode, Class: class}
	}
	var token struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &token); err != nil {
		return contracts.TokenBundle{}, fmt.Errorf("decode Copilot token response: %w", err)
	}
	if strings.TrimSpace(token.Token) == "" {
		return contracts.TokenBundle{}, fmt.Errorf("%w: Copilot token response has no token", contracts.ErrInvalidContract)
	}
	current.AccessToken = token.Token
	if token.ExpiresAt > 0 {
		current.ExpiresAt = time.Unix(token.ExpiresAt, 0).UTC()
	}
	return current, nil
}

func (p *Provider) Revoke(context.Context, contracts.Credential) error {
	return contracts.ErrUnsupportedCapability
}

func (p *Provider) Profile(contracts.Account) contracts.UpstreamProfile {
	return contracts.UpstreamProfile{
		BaseURL:       p.Config.APIBaseURL,
		Protocol:      "openai_chat",
		InferencePath: p.Config.InferencePath,
		UserAgent:     "GitHubCopilotChat/0.44.0",
		ExtraHeaders: map[string]string{
			"copilot-integration-id":              "vscode-chat",
			"editor-version":                      "vscode/1.109.3",
			"editor-plugin-version":               "copilot-chat/0.44.0",
			"openai-intent":                       "conversation-panel",
			"x-github-api-version":                "2025-05-01",
			"x-vscode-user-agent-library-version": "electron-fetch",
		},
	}
}

func (p *Provider) Quota(context.Context, contracts.Credential) (contracts.QuotaInfo, error) {
	return contracts.QuotaInfo{}, nil
}
