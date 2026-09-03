package kiro

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

const maxRefreshResponseBytes = 1 << 20

type Provider struct {
	Client *http.Client
	Config providerapi.EndpointProfile
}

func NewOAuth(client *http.Client) *Provider {
	return &Provider{Client: client, Config: providerapi.KiroOAuthProfile()}
}

func (p *Provider) Kind() string { return providerapi.KindKiro }

func (p *Provider) AuthMode() string { return p.Config.AuthMode }

func (p *Provider) Authorize(_ context.Context, req contracts.ImportRequest) (contracts.TokenBundle, error) {
	if req.Provider != "" && req.Provider != p.Kind() {
		return contracts.TokenBundle{}, fmt.Errorf("%w: provider mismatch", contracts.ErrInvalidContract)
	}
	if err := providerapi.ValidateImportAuthMode(p.Config, req.AuthMode); err != nil {
		return contracts.TokenBundle{}, err
	}
	if req.TokenBundle == nil || strings.TrimSpace(req.TokenBundle.AccessToken) == "" {
		return contracts.TokenBundle{}, fmt.Errorf("%w: imported Kiro token bundle requires access_token", contracts.ErrInvalidContract)
	}
	bundle := *req.TokenBundle
	bundle.Metadata = cloneMetadata(bundle.Metadata)
	for _, key := range []string{"profile_arn", "machine_id", "region"} {
		if value := strings.TrimSpace(req.Metadata[key]); value != "" {
			bundle.Metadata[key] = value
		}
	}
	return bundle, nil
}

func (p *Provider) Refresh(_ context.Context, _ contracts.Credential) (contracts.TokenBundle, error) {
	return contracts.TokenBundle{}, providerapi.ErrOAuthRefreshUnsupported
}

func (p *Provider) RefreshOAuth(ctx context.Context, current contracts.TokenBundle) (contracts.TokenBundle, error) {
	refreshToken := strings.TrimSpace(current.RefreshToken)
	if refreshToken == "" {
		return contracts.TokenBundle{}, fmt.Errorf("%w: Kiro refresh_token is missing", contracts.ErrInvalidContract)
	}
	if err := p.Config.Validate(); err != nil {
		return contracts.TokenBundle{}, err
	}
	body, err := json.Marshal(map[string]string{"refreshToken": refreshToken})
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Config.OAuthTokenURL, bytes.NewReader(body))
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	fingerprint := strings.TrimSpace(current.Metadata["machine_id"])
	if fingerprint == "" {
		return contracts.TokenBundle{}, fmt.Errorf("%w: Kiro machine_id is missing", contracts.ErrInvalidContract)
	}
	req.Header.Set("User-Agent", "KiroIDE-0.12.155-"+fingerprint)
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRefreshResponseBytes+1))
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	if len(responseBody) > maxRefreshResponseBytes {
		return contracts.TokenBundle{}, fmt.Errorf("Kiro refresh response exceeds %d bytes", maxRefreshResponseBytes)
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
		return contracts.TokenBundle{}, &providerapi.HTTPError{Operation: "Kiro refresh", StatusCode: resp.StatusCode, Class: class}
	}
	var token struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
	}
	if err := json.Unmarshal(responseBody, &token); err != nil {
		return contracts.TokenBundle{}, fmt.Errorf("decode Kiro refresh response: %w", err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return contracts.TokenBundle{}, fmt.Errorf("%w: Kiro refresh response has no accessToken", contracts.ErrInvalidContract)
	}
	current.AccessToken = token.AccessToken
	if token.RefreshToken != "" {
		current.RefreshToken = token.RefreshToken
	}
	if token.ExpiresIn > 0 {
		current.ExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	}
	return current, nil
}

func (p *Provider) Revoke(context.Context, contracts.Credential) error {
	return contracts.ErrUnsupportedCapability
}

func (p *Provider) Profile(contracts.Account) contracts.UpstreamProfile {
	return contracts.UpstreamProfile{BaseURL: p.Config.APIBaseURL, Protocol: "openai_chat", InferencePath: p.Config.InferencePath}
}

func (p *Provider) ProfileForImport(account contracts.Account, req contracts.ImportRequest) (contracts.UpstreamProfile, error) {
	profileARN := strings.TrimSpace(req.Metadata["profile_arn"])
	machineID := strings.TrimSpace(req.Metadata["machine_id"])
	if profileARN == "" || len(profileARN) > 2048 || strings.IndexFunc(profileARN, unicode.IsControl) >= 0 {
		return contracts.UpstreamProfile{}, fmt.Errorf("%w: valid Kiro profile_arn is required", contracts.ErrInvalidContract)
	}
	if machineID == "" || len(machineID) > 256 || strings.IndexFunc(machineID, unicode.IsControl) >= 0 {
		return contracts.UpstreamProfile{}, fmt.Errorf("%w: valid Kiro machine_id is required", contracts.ErrInvalidContract)
	}
	profile := p.Profile(account)
	profile.UserAgent = "aws-sdk-js/1.0.34 ua/2.1 os/linux lang/js md/nodejs#22.21.1 api/codewhispererstreaming#1.0.34 m/E KiroIDE-0.12.155-" + machineID
	profile.ExtraHeaders = map[string]string{
		"x-amz-user-agent":            "aws-sdk-js/1.0.34 KiroIDE-0.12.155-" + machineID,
		"X-Amz-Target":                "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
		"x-amzn-codewhisperer-optout": "true",
		"x-amzn-kiro-agent-mode":      "vibe",
	}
	profile.Metadata = map[string]string{"profile_arn": profileARN, "machine_id": machineID}
	return profile, nil
}

func cloneMetadata(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source)+3)
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func (p *Provider) Quota(context.Context, contracts.Credential) (contracts.QuotaInfo, error) {
	// getUsageLimits includes decimal precision fields that the stable integer
	// quota contract cannot represent losslessly. Keep it missing for now.
	return contracts.QuotaInfo{}, nil
}
