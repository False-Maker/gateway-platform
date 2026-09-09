package provider

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

const (
	KindCodex       = "codex"
	KindClaude      = "claude"
	KindGemini      = "gemini"
	KindGrok        = "grok"
	KindCopilot     = "copilot"
	KindAntigravity = "antigravity"
	KindKiro        = "kiro"
	KindWindsurf    = "windsurf"

	AuthModeOAuth  = "oauth"
	AuthModeAPIKey = "api_key"
)

var ErrInvalidProfile = errors.New("invalid provider profile")

// EndpointProfile contains only public endpoint and header policy. Secrets
// are supplied per request and are never retained here.
type EndpointProfile struct {
	Kind              string
	AuthMode          string
	OAuthAuthorizeURL string
	OAuthTokenURL     string
	OAuthRevokeURL    string
	ClientID          string
	APIBaseURL        string
	InferencePath     string
	QuotaPath         string
	OAuthBetaHeader   string
	// TLSFingerprint names a gateway ClientHello profile (internal/gateway
	// tlsprofile.go). Empty means standard crypto/tls.
	TLSFingerprint string
}

func CodexChatGPTProfile() EndpointProfile {
	return EndpointProfile{
		Kind:              KindCodex,
		AuthMode:          AuthModeOAuth,
		OAuthAuthorizeURL: "https://auth.openai.com/oauth/authorize",
		OAuthTokenURL:     "https://auth.openai.com/oauth/token",
		OAuthRevokeURL:    "https://auth.openai.com/oauth/revoke",
		ClientID:          "app_EMoamEEZ73f0CkXaXp7hrann",
		APIBaseURL:        "https://chatgpt.com/backend-api/codex",
		InferencePath:     "/responses",
		TLSFingerprint:    "codex_rustls",
	}
}

func CodexAPIKeyProfile() EndpointProfile {
	return EndpointProfile{
		Kind:          KindCodex,
		AuthMode:      AuthModeAPIKey,
		APIBaseURL:    "https://api.openai.com/v1",
		InferencePath: "/responses",
	}
}

func ClaudeConsoleOAuthProfile() EndpointProfile {
	return EndpointProfile{
		Kind:              KindClaude,
		AuthMode:          AuthModeOAuth,
		OAuthAuthorizeURL: "https://platform.claude.com/oauth/authorize",
		OAuthTokenURL:     "https://platform.claude.com/v1/oauth/token",
		ClientID:          "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		APIBaseURL:        "https://api.anthropic.com",
		InferencePath:     "/v1/messages",
		OAuthBetaHeader:   "oauth-2025-04-20",
		TLSFingerprint:    "node24",
	}
}

func ClaudeAIOAuthProfile() EndpointProfile {
	profile := ClaudeConsoleOAuthProfile()
	profile.OAuthAuthorizeURL = "https://claude.com/cai/oauth/authorize"
	return profile
}

func ClaudeAPIKeyProfile() EndpointProfile {
	return EndpointProfile{
		Kind:          KindClaude,
		AuthMode:      AuthModeAPIKey,
		APIBaseURL:    "https://api.anthropic.com",
		InferencePath: "/v1/messages",
	}
}

// GeminiAPIKeyProfile describes the public GenerateContent transport. The
// model is supplied by the account's inference path template, so one profile
// can serve multiple Gemini models without storing a guessed default model.
func GeminiAPIKeyProfile() EndpointProfile {
	return EndpointProfile{Kind: KindGemini, AuthMode: AuthModeAPIKey, APIBaseURL: "https://generativelanguage.googleapis.com", InferencePath: "/v1beta/models/{model}:generateContent"}
}

// GrokAPIKeyProfile describes xAI's OpenAI-compatible Chat Completions API.
func GrokAPIKeyProfile() EndpointProfile {
	return EndpointProfile{Kind: KindGrok, AuthMode: AuthModeAPIKey, APIBaseURL: "https://api.x.ai", InferencePath: "/v1/chat/completions"}
}

// GitHubCopilotProfile describes the official Copilot Chat transport. Device
// authorization remains a wrapper concern; the provider imports the resulting
// short-lived Copilot token and retains the GitHub token for refresh.
func GitHubCopilotProfile() EndpointProfile {
	return EndpointProfile{
		Kind:              KindCopilot,
		AuthMode:          AuthModeOAuth,
		OAuthAuthorizeURL: "https://github.com/login/device/code",
		OAuthTokenURL:     "https://api.github.com/copilot_internal/v2/token",
		ClientID:          "Iv1.b507a08c87ecfe98",
		APIBaseURL:        "https://api.githubcopilot.com",
		InferencePath:     "/chat/completions",
	}
}

func AntigravityOAuthProfile() EndpointProfile {
	return EndpointProfile{
		Kind:              KindAntigravity,
		AuthMode:          AuthModeOAuth,
		OAuthAuthorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		OAuthTokenURL:     "https://oauth2.googleapis.com/token",
		ClientID:          "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com",
		APIBaseURL:        "https://cloudcode-pa.googleapis.com",
		InferencePath:     "/v1internal:generateContent",
	}
}

func KiroOAuthProfile() EndpointProfile {
	return EndpointProfile{
		Kind:          KindKiro,
		AuthMode:      AuthModeOAuth,
		OAuthTokenURL: "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken",
		APIBaseURL:    "https://q.us-east-1.amazonaws.com",
		InferencePath: "/generateAssistantResponse",
	}
}

func WindsurfAPIKeyProfile() EndpointProfile {
	return EndpointProfile{Kind: KindWindsurf, AuthMode: AuthModeAPIKey, APIBaseURL: "https://server.codeium.com"}
}

func ValidateImportAuthMode(profile EndpointProfile, requested string) error {
	if strings.TrimSpace(requested) == "" {
		return nil
	}
	want, ok := canonicalAuthMode(requested)
	if !ok || want != profile.AuthMode {
		return fmt.Errorf("%w: auth mode %q does not match %s profile", contracts.ErrInvalidContract, requested, profile.AuthMode)
	}
	return nil
}

func (p EndpointProfile) Validate() error {
	if p.Kind != KindCodex && p.Kind != KindClaude && p.Kind != KindGemini && p.Kind != KindGrok && p.Kind != KindCopilot && p.Kind != KindAntigravity && p.Kind != KindKiro && p.Kind != KindWindsurf {
		return fmt.Errorf("%w: unsupported provider kind", ErrInvalidProfile)
	}
	if p.AuthMode != AuthModeOAuth && p.AuthMode != AuthModeAPIKey {
		return fmt.Errorf("%w: kind or auth mode", ErrInvalidProfile)
	}
	if _, err := parseHTTPURL(p.APIBaseURL); err != nil {
		return fmt.Errorf("%w: api base URL: %v", ErrInvalidProfile, err)
	}
	if !strings.HasPrefix(p.InferencePath, "/") {
		return fmt.Errorf("%w: inference path must start with slash", ErrInvalidProfile)
	}
	if p.QuotaPath != "" && !strings.HasPrefix(p.QuotaPath, "/") {
		return fmt.Errorf("%w: quota path must start with slash", ErrInvalidProfile)
	}
	if p.AuthMode == AuthModeOAuth {
		if p.Kind == KindGemini || p.Kind == KindGrok {
			return fmt.Errorf("%w: %s OAuth is not supported", ErrInvalidProfile, p.Kind)
		}
		if p.Kind == KindKiro {
			if _, err := parseHTTPURL(p.OAuthTokenURL); err != nil {
				return fmt.Errorf("%w: oauth token URL: %v", ErrInvalidProfile, err)
			}
			return nil
		}
		for name, raw := range map[string]string{
			"oauth authorize URL": p.OAuthAuthorizeURL,
			"oauth token URL":     p.OAuthTokenURL,
		} {
			if _, err := parseHTTPURL(raw); err != nil {
				return fmt.Errorf("%w: %s: %v", ErrInvalidProfile, name, err)
			}
		}
		if strings.TrimSpace(p.ClientID) == "" {
			return fmt.Errorf("%w: OAuth client ID is empty", ErrInvalidProfile)
		}
		if p.OAuthRevokeURL != "" {
			if _, err := parseHTTPURL(p.OAuthRevokeURL); err != nil {
				return fmt.Errorf("%w: oauth revoke URL: %v", ErrInvalidProfile, err)
			}
		}
	}
	return nil
}

func (p EndpointProfile) InferenceURL() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	return strings.TrimRight(p.APIBaseURL, "/") + p.InferencePath, nil
}

// QuotaURL returns the optional provider quota endpoint. An empty QuotaPath is
// a legal profile state for providers that do not expose quota information.
func (p EndpointProfile) QuotaURL() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	if p.QuotaPath == "" {
		return "", nil
	}
	return strings.TrimRight(p.APIBaseURL, "/") + p.QuotaPath, nil
}

func (p EndpointProfile) InferenceHeaders(accessToken string) (http.Header, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(accessToken) == "" {
		return nil, fmt.Errorf("%w: access token is empty", ErrInvalidProfile)
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	if p.Kind == KindClaude && p.AuthMode == AuthModeAPIKey {
		headers.Set("x-api-key", accessToken)
	} else if p.Kind == KindGemini && p.AuthMode == AuthModeAPIKey {
		headers.Set("x-goog-api-key", accessToken)
	} else {
		headers.Set("Authorization", "Bearer "+accessToken)
	}
	if p.OAuthBetaHeader != "" && p.AuthMode == AuthModeOAuth {
		headers.Set("anthropic-beta", p.OAuthBetaHeader)
	}
	if p.Kind == KindClaude {
		headers.Set("anthropic-version", "2023-06-01")
	}
	if p.Kind == KindCopilot {
		headers.Set("User-Agent", "GitHubCopilotChat/0.44.0")
		headers.Set("copilot-integration-id", "vscode-chat")
		headers.Set("editor-version", "vscode/1.109.3")
		headers.Set("editor-plugin-version", "copilot-chat/0.44.0")
		headers.Set("openai-intent", "conversation-panel")
		headers.Set("x-github-api-version", "2025-05-01")
		headers.Set("x-vscode-user-agent-library-version", "electron-fetch")
		var requestID [16]byte
		if _, err := rand.Read(requestID[:]); err != nil {
			return nil, fmt.Errorf("create Copilot request ID: %w", err)
		}
		id := hex.EncodeToString(requestID[:])
		headers.Set("x-request-id", id)
		headers.Set("x-interaction-id", id)
		headers.Set("x-agent-task-id", id)
		headers.Set("x-interaction-type", "conversation-panel")
		headers.Set("X-Initiator", "user")
	}
	return headers, nil
}

func parseHTTPURL(raw string) (*url.URL, error) {
	if len(raw) > 2048 {
		return nil, errors.New("URL exceeds 2048 bytes")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("must be an absolute HTTP(S) URL")
	}
	return u, nil
}
