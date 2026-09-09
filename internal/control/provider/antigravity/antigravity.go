package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

const defaultUserAgent = "antigravity/1.20.5 linux/x64"
const maxRefreshResponseBytes = 1 << 20

type Provider struct {
	Client *http.Client
	HTTP   providerapi.HTTPClient
	Config providerapi.EndpointProfile
}

func NewOAuth(client *http.Client) *Provider {
	return &Provider{Client: client, HTTP: providerapi.HTTPClient{Client: client}, Config: providerapi.AntigravityOAuthProfile()}
}

func (p *Provider) Kind() string { return providerapi.KindAntigravity }

func (p *Provider) AuthMode() string { return p.Config.AuthMode }

func (p *Provider) Authorize(_ context.Context, req contracts.ImportRequest) (contracts.TokenBundle, error) {
	if req.Provider != "" && req.Provider != p.Kind() {
		return contracts.TokenBundle{}, fmt.Errorf("%w: provider mismatch", contracts.ErrInvalidContract)
	}
	if err := providerapi.ValidateImportAuthMode(p.Config, req.AuthMode); err != nil {
		return contracts.TokenBundle{}, err
	}
	if req.TokenBundle == nil || strings.TrimSpace(req.TokenBundle.AccessToken) == "" {
		return contracts.TokenBundle{}, fmt.Errorf("%w: imported Antigravity token bundle requires access_token", contracts.ErrInvalidContract)
	}
	bundle := *req.TokenBundle
	if secret := strings.TrimSpace(req.Metadata["client_secret"]); secret != "" {
		bundle.Metadata = make(map[string]string, len(req.TokenBundle.Metadata)+1)
		for key, value := range req.TokenBundle.Metadata {
			bundle.Metadata[key] = value
		}
		bundle.Metadata["client_secret"] = secret
	}
	return bundle, nil
}

func (p *Provider) Refresh(_ context.Context, _ contracts.Credential) (contracts.TokenBundle, error) {
	return contracts.TokenBundle{}, providerapi.ErrOAuthRefreshUnsupported
}

func (p *Provider) RefreshOAuth(ctx context.Context, current contracts.TokenBundle) (contracts.TokenBundle, error) {
	if strings.TrimSpace(current.RefreshToken) == "" {
		return contracts.TokenBundle{}, fmt.Errorf("%w: Antigravity refresh_token is missing", contracts.ErrInvalidContract)
	}
	clientSecret := strings.TrimSpace(current.Metadata["client_secret"])
	if clientSecret == "" {
		return contracts.TokenBundle{}, fmt.Errorf("%w: Antigravity OAuth client_secret is missing", contracts.ErrInvalidContract)
	}
	values := url.Values{
		"client_id":     {p.Config.ClientID},
		"client_secret": {clientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {current.RefreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Config.OAuthTokenURL, bytes.NewBufferString(values.Encode()))
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	client, err = (providerapi.HTTPClient{Client: client}).ClientForProxy(current.Metadata["proxy"])
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRefreshResponseBytes+1))
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	if len(body) > maxRefreshResponseBytes {
		return contracts.TokenBundle{}, fmt.Errorf("Antigravity refresh response exceeds %d bytes", maxRefreshResponseBytes)
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
		return contracts.TokenBundle{}, &providerapi.HTTPError{Operation: "Antigravity refresh", StatusCode: resp.StatusCode, Class: class}
	}
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &token); err != nil {
		return contracts.TokenBundle{}, fmt.Errorf("decode Antigravity refresh response: %w", err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return contracts.TokenBundle{}, fmt.Errorf("%w: Antigravity refresh response has no access_token", contracts.ErrInvalidContract)
	}
	current.AccessToken = token.AccessToken
	if token.RefreshToken != "" {
		current.RefreshToken = token.RefreshToken
	}
	if token.ExpiresIn > 0 {
		current.ExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	}
	if token.Scope != "" {
		current.Scopes = strings.Fields(token.Scope)
	}
	return current, nil
}

func (p *Provider) Revoke(context.Context, contracts.Credential) error {
	return contracts.ErrUnsupportedCapability
}

func (p *Provider) Profile(contracts.Account) contracts.UpstreamProfile {
	return contracts.UpstreamProfile{
		BaseURL:       p.Config.APIBaseURL,
		Protocol:      "gemini_generate",
		InferencePath: p.Config.InferencePath,
		UserAgent:     defaultUserAgent,
	}
}

func (p *Provider) ProfileForImport(account contracts.Account, req contracts.ImportRequest) (contracts.UpstreamProfile, error) {
	projectID := strings.TrimSpace(req.Metadata["project_id"])
	if projectID == "" {
		return contracts.UpstreamProfile{}, fmt.Errorf("%w: Antigravity project_id is required", contracts.ErrInvalidContract)
	}
	if len(projectID) > 256 || strings.IndexFunc(projectID, unicode.IsControl) >= 0 {
		return contracts.UpstreamProfile{}, fmt.Errorf("%w: Antigravity project_id is invalid", contracts.ErrInvalidContract)
	}
	profile := p.Profile(account)
	profile.ExtraHeaders = map[string]string{"X-Goog-User-Project": projectID}
	return profile, nil
}

func (p *Provider) Quota(ctx context.Context, credential contracts.Credential) (contracts.QuotaInfo, error) {
	project := strings.TrimSpace(credential.Metadata["project_id"])
	if project == "" {
		return contracts.QuotaInfo{}, nil
	}
	endpoint := strings.TrimRight(p.Config.APIBaseURL, "/") + "/v1internal:fetchAvailableModels"
	headers, err := p.Config.InferenceHeaders(credential.AccessToken)
	if err != nil {
		return contracts.QuotaInfo{}, err
	}
	headers.Set("Content-Type", "application/json")
	headers.Set("User-Agent", defaultUserAgent)
	body, err := json.Marshal(map[string]string{"project": project})
	if err != nil {
		return contracts.QuotaInfo{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return contracts.QuotaInfo{}, err
	}
	req.Header = headers
	client, err := p.HTTP.ClientForProxy(credential.Proxy)
	if err != nil {
		return contracts.QuotaInfo{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return contracts.QuotaInfo{}, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRefreshResponseBytes+1))
	if err != nil {
		return contracts.QuotaInfo{}, err
	}
	if len(responseBody) > maxRefreshResponseBytes {
		return contracts.QuotaInfo{}, fmt.Errorf("Antigravity quota response exceeds %d bytes", maxRefreshResponseBytes)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return contracts.QuotaInfo{}, &providerapi.HTTPError{Operation: "Antigravity quota", StatusCode: resp.StatusCode, Class: quotaHTTPErrorClass(resp.StatusCode)}
	}
	return ParseAvailableModelsQuota(responseBody)
}

func quotaHTTPErrorClass(status int) contracts.ErrorClass {
	switch {
	case status == http.StatusUnauthorized:
		return contracts.ErrorAuthInvalid
	case status == http.StatusForbidden:
		return contracts.ErrorForbiddenCapability
	case status == http.StatusTooManyRequests:
		return contracts.ErrorRateLimitedKnown
	case status >= http.StatusInternalServerError:
		return contracts.ErrorUpstream5xx
	default:
		return contracts.ErrorForbiddenCapability
	}
}

type availableModelsResponse struct {
	Models map[string]struct {
		QuotaInfo *struct {
			RemainingFraction json.RawMessage `json:"remainingFraction"`
			ResetTime         string          `json:"resetTime"`
		} `json:"quotaInfo"`
	} `json:"models"`
}

// ParseAvailableModelsQuota maps only the documented per-model fraction and
// reset fields. Missing quotaInfo or missing remainingFraction stays unknown.
func ParseAvailableModelsQuota(body []byte) (contracts.QuotaInfo, error) {
	var response availableModelsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return contracts.QuotaInfo{}, fmt.Errorf("decode Antigravity quota: %w", err)
	}
	if response.Models == nil {
		return contracts.QuotaInfo{}, nil
	}
	items := make([]contracts.QuotaItem, 0, len(response.Models))
	for model, entry := range response.Models {
		if entry.QuotaInfo == nil {
			continue
		}
		fraction, ok := parseDecimal(entry.QuotaInfo.RemainingFraction)
		if !ok {
			continue
		}
		item := contracts.QuotaItem{Scope: "model", Model: model, Unit: "fraction", RemainingFraction: fraction}
		if raw := strings.TrimSpace(entry.QuotaInfo.ResetTime); raw != "" {
			reset, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return contracts.QuotaInfo{}, fmt.Errorf("decode Antigravity quota reset time: %w", err)
			}
			item.ResetAt = &reset
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return contracts.QuotaInfo{}, nil
	}
	quota := contracts.QuotaInfo{Items: items}
	if err := providerapi.ValidateQuotaInfo(quota); err != nil {
		return contracts.QuotaInfo{}, err
	}
	return quota, nil
}

func parseDecimal(raw json.RawMessage) (*contracts.Decimal, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var value contracts.Decimal
	if err := json.Unmarshal(raw, &value); err != nil || !value.Valid() {
		return nil, false
	}
	return &value, true
}
