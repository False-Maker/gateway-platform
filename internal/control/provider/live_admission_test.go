package provider_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/internal/control/provider/claude"
	"github.com/elucid/gateway-platform/internal/control/provider/codex"
	"github.com/elucid/gateway-platform/internal/control/provider/copilot"
	"github.com/elucid/gateway-platform/internal/control/provider/gemini"
	"github.com/elucid/gateway-platform/internal/control/provider/grok"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/elucid/gateway-platform/pkg/protokit"
)

// Live admission is deliberately opt-in. It is safe to run the normal test
// suite without credentials and impossible to mistake a fixture for a live
// provider result.
const liveAdmissionOptIn = "1"

type liveConfig struct {
	Provider       providerapi.Provider
	Profile        providerapi.EndpointProfile
	Protocol       protokit.Protocol
	AccessToken    string
	RefreshToken   string
	Model          string
	MaxOutput      int
	QuotaPath      string
	SetProfile     func(providerapi.EndpointProfile)
	ProviderName   string
	Authentication string
}

func TestLiveProviderInferenceAndUsageAdmission(t *testing.T) {
	cfg := loadLiveConfig(t)
	profile := cfg.Profile
	if cfg.ProviderName == providerapi.KindGemini {
		profile.InferencePath = strings.ReplaceAll(profile.InferencePath, "{model}", cfg.Model)
	}

	payload, err := liveInferencePayload(cfg.Protocol, cfg.Model, cfg.MaxOutput)
	if err != nil {
		t.Fatal(err)
	}
	body, err := (providerapi.HTTPClient{}).Inference(context.Background(), profile, cfg.AccessToken, payload)
	if err != nil {
		t.Fatalf("live %s inference failed: %v", cfg.ProviderName, err)
	}
	usage, present, err := protokit.ParseUsage(body, cfg.Protocol)
	if err != nil {
		t.Fatalf("live %s usage decode failed: %v", cfg.ProviderName, err)
	}
	if !present {
		t.Fatalf("live %s response did not include usage; body was intentionally not logged", cfg.ProviderName)
	}
	if usage.Input+usage.CacheRead+usage.CacheWrite <= 0 || usage.Output <= 0 {
		t.Fatalf("live %s returned incomplete usage: %+v", cfg.ProviderName, usage)
	}
	if err := validateLiveInferenceResponse(body, cfg.Protocol); err != nil {
		t.Fatalf("live %s response was not admitted: %v; body was intentionally not logged", cfg.ProviderName, err)
	}
}

func TestLiveProviderOAuthRefreshAdmission(t *testing.T) {
	cfg := loadLiveConfig(t)
	if cfg.Authentication != providerapi.AuthModeOAuth {
		t.Skip("live OAuth refresh requires GATEWAY_LIVE_AUTH_MODE=oauth")
	}
	if cfg.RefreshToken == "" {
		t.Skip("GATEWAY_LIVE_REFRESH_TOKEN is not set; inference/usage can run with access token only")
	}
	refreshed, err := providerapi.RefreshOAuth(context.Background(), cfg.Provider, contracts.TokenBundle{RefreshToken: cfg.RefreshToken})
	if err != nil {
		t.Fatalf("live %s OAuth refresh failed: %v", cfg.ProviderName, err)
	}
	if strings.TrimSpace(refreshed.AccessToken) == "" {
		t.Fatalf("live %s OAuth refresh returned no access token", cfg.ProviderName)
	}
}

func TestLiveProviderQuotaAdmission(t *testing.T) {
	cfg := loadLiveConfig(t)
	if cfg.QuotaPath == "" {
		t.Skip("GATEWAY_LIVE_QUOTA_PATH is not set; quota endpoint/schema remains unverified")
	}
	cfg.Profile.QuotaPath = cfg.QuotaPath
	cfg.SetProfile(cfg.Profile)
	quota, err := cfg.Provider.Quota(context.Background(), contracts.Credential{Kind: cfg.Authentication, AccessToken: cfg.AccessToken})
	if err != nil {
		t.Fatalf("live %s quota failed: %v", cfg.ProviderName, err)
	}
	if len(quota.Items) == 0 {
		t.Fatalf("live %s quota endpoint returned no quota items; schema is not admitted", cfg.ProviderName)
	}
	remainingPresent := false
	for i, item := range quota.Items {
		if item.Remaining != nil && *item.Remaining < 0 {
			t.Fatalf("live %s quota item %d has negative remaining", cfg.ProviderName, i)
		}
		remainingPresent = remainingPresent || item.Remaining != nil
	}
	if !remainingPresent {
		t.Fatalf("live %s quota endpoint returned no remaining values; schema is not admitted", cfg.ProviderName)
	}
}

func loadLiveConfig(t *testing.T) liveConfig {
	t.Helper()
	if os.Getenv("GATEWAY_RUN_LIVE_PROVIDER_TESTS") != liveAdmissionOptIn {
		t.Skip("live provider tests are opt-in: set GATEWAY_RUN_LIVE_PROVIDER_TESTS=1")
	}
	if os.Getenv("GATEWAY_LIVE_CONFIRM") != "provider-admission" {
		t.Skip("set GATEWAY_LIVE_CONFIRM=provider-admission to authorize live provider requests")
	}
	kind := strings.ToLower(strings.TrimSpace(os.Getenv("GATEWAY_LIVE_PROVIDER")))
	if kind != providerapi.KindCodex && kind != providerapi.KindClaude && kind != providerapi.KindGemini && kind != providerapi.KindGrok && kind != providerapi.KindCopilot {
		t.Fatalf("GATEWAY_LIVE_PROVIDER must be codex, claude, gemini, grok, or copilot")
	}
	authentication := strings.ToLower(strings.TrimSpace(os.Getenv("GATEWAY_LIVE_AUTH_MODE")))
	if authentication == "" && (kind == providerapi.KindGemini || kind == providerapi.KindGrok) {
		authentication = providerapi.AuthModeAPIKey
	}
	if authentication != providerapi.AuthModeOAuth && authentication != providerapi.AuthModeAPIKey {
		t.Fatalf("GATEWAY_LIVE_AUTH_MODE must be oauth or api_key")
	}
	if (kind == providerapi.KindGemini || kind == providerapi.KindGrok) && authentication != providerapi.AuthModeAPIKey {
		t.Fatalf("%s live admission only supports api_key", kind)
	}
	if kind == providerapi.KindCopilot && authentication != providerapi.AuthModeOAuth {
		t.Fatalf("copilot live admission only supports oauth")
	}
	accessToken := strings.TrimSpace(os.Getenv("GATEWAY_LIVE_ACCESS_TOKEN"))
	if accessToken == "" {
		t.Fatalf("GATEWAY_LIVE_ACCESS_TOKEN is required when live admission is enabled")
	}
	model := strings.TrimSpace(os.Getenv("GATEWAY_LIVE_MODEL"))
	if model == "" {
		t.Fatalf("GATEWAY_LIVE_MODEL is required when live admission is enabled")
	}
	maxOutput, err := strconv.Atoi(os.Getenv("GATEWAY_LIVE_MAX_OUTPUT_TOKENS"))
	if err != nil || maxOutput <= 0 {
		t.Fatalf("GATEWAY_LIVE_MAX_OUTPUT_TOKENS must be a positive integer")
	}

	var cfg liveConfig
	switch {
	case kind == providerapi.KindCodex && authentication == providerapi.AuthModeOAuth:
		p := codex.NewOAuth(nil)
		cfg = liveConfig{Provider: p, Profile: p.Config, Protocol: protokit.OpenAIResponses, SetProfile: func(profile providerapi.EndpointProfile) { p.Config = profile }}
	case kind == providerapi.KindCodex:
		p := codex.NewAPIKey(nil)
		cfg = liveConfig{Provider: p, Profile: p.Config, Protocol: protokit.OpenAIResponses, SetProfile: func(profile providerapi.EndpointProfile) { p.Config = profile }}
	case kind == providerapi.KindClaude && authentication == providerapi.AuthModeOAuth:
		p := claude.NewConsoleOAuth(nil)
		cfg = liveConfig{Provider: p, Profile: p.Config, Protocol: protokit.AnthropicMessages, SetProfile: func(profile providerapi.EndpointProfile) { p.Config = profile }}
	case kind == providerapi.KindClaude:
		p := claude.NewAPIKey(nil)
		cfg = liveConfig{Provider: p, Profile: p.Config, Protocol: protokit.AnthropicMessages, SetProfile: func(profile providerapi.EndpointProfile) { p.Config = profile }}
	case kind == providerapi.KindGrok:
		p := grok.NewAPIKey(nil)
		cfg = liveConfig{Provider: p, Profile: p.Config, Protocol: protokit.OpenAIChat, SetProfile: func(profile providerapi.EndpointProfile) { p.Config = profile }}
	case kind == providerapi.KindCopilot:
		p := copilot.NewOAuth(nil)
		cfg = liveConfig{Provider: p, Profile: p.Config, Protocol: protokit.OpenAIChat, SetProfile: func(profile providerapi.EndpointProfile) { p.Config = profile }}
	default:
		p := gemini.NewAPIKey(nil)
		cfg = liveConfig{Provider: p, Profile: p.Config, Protocol: protokit.GeminiGenerate, SetProfile: func(profile providerapi.EndpointProfile) { p.Config = profile }}
	}
	cfg.ProviderName = kind
	cfg.Authentication = authentication
	cfg.AccessToken = accessToken
	cfg.RefreshToken = strings.TrimSpace(os.Getenv("GATEWAY_LIVE_REFRESH_TOKEN"))
	cfg.Model = model
	cfg.MaxOutput = maxOutput
	cfg.QuotaPath = strings.TrimSpace(os.Getenv("GATEWAY_LIVE_QUOTA_PATH"))
	if baseURL := strings.TrimSpace(os.Getenv("GATEWAY_LIVE_API_BASE_URL")); baseURL != "" {
		if err := validateLiveAPIBaseURL(baseURL, cfg.Profile.APIBaseURL); err != nil {
			t.Fatalf("GATEWAY_LIVE_API_BASE_URL must match the audited %s provider base URL; custom or fixture endpoints are not live admission", kind)
		}
	}
	if cfg.QuotaPath != "" && !strings.HasPrefix(cfg.QuotaPath, "/") {
		t.Fatalf("GATEWAY_LIVE_QUOTA_PATH must start with /")
	}
	cfg.SetProfile(cfg.Profile)
	return cfg
}

func validateLiveAPIBaseURL(override, audited string) error {
	if strings.TrimRight(strings.TrimSpace(override), "/") != strings.TrimRight(strings.TrimSpace(audited), "/") {
		return fmt.Errorf("custom base URL is not allowed")
	}
	return nil
}

func validateLiveInferenceResponse(body []byte, protocol protokit.Protocol) error {
	canonical, err := protokit.ConvertResponse(body, protocol, protokit.OpenAIChat)
	if err != nil {
		return err
	}
	var response struct {
		Choices []struct {
			Message struct {
				Content *string `json:"content"`
			} `json:"message"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(canonical, &response); err != nil {
		return err
	}
	if len(response.Choices) != 1 || response.Choices[0].Message.Content == nil || strings.TrimSpace(*response.Choices[0].Message.Content) == "" {
		return fmt.Errorf("response must contain exactly one non-empty text choice")
	}
	if response.Choices[0].FinishReason == nil || strings.TrimSpace(*response.Choices[0].FinishReason) == "" {
		return fmt.Errorf("response must contain a finish reason")
	}
	return nil
}

func TestLiveAdmissionRejectsCustomBaseURL(t *testing.T) {
	if err := validateLiveAPIBaseURL("http://127.0.0.1:12345", "https://api.openai.com/v1"); err == nil {
		t.Fatal("custom base URL was accepted for live admission")
	}
}

func liveInferencePayload(protocol protokit.Protocol, model string, maxOutput int) ([]byte, error) {
	var payload any
	switch protocol {
	case protokit.OpenAIChat:
		payload = map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "Reply with the single word live."}}, "max_tokens": maxOutput}
	case protokit.OpenAIResponses:
		payload = map[string]any{"model": model, "input": "Reply with the single word live.", "max_output_tokens": maxOutput}
	case protokit.AnthropicMessages:
		payload = map[string]any{"model": model, "max_tokens": maxOutput, "messages": []map[string]string{{"role": "user", "content": "Reply with the single word live."}}}
	case protokit.GeminiGenerate:
		payload = map[string]any{"contents": []map[string]any{{"role": "user", "parts": []map[string]string{{"text": "Reply with the single word live."}}}}, "generationConfig": map[string]any{"maxOutputTokens": maxOutput}}
	default:
		return nil, fmt.Errorf("unsupported live protocol %q", protocol)
	}
	return json.Marshal(payload)
}
