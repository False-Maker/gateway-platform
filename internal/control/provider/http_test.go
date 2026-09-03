package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

func TestOAuthFixturesVerifyCodexAndClaudeTokenShapes(t *testing.T) {
	profiles := []EndpointProfile{CodexChatGPTProfile(), ClaudeConsoleOAuthProfile()}
	for _, original := range profiles {
		profile := original
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/oauth/token" {
				t.Fatalf("token path = %s", r.URL.Path)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			if profile.Kind == KindCodex {
				values, err := url.ParseQuery(string(body))
				if err != nil || values.Get("grant_type") != "authorization_code" || values.Get("client_id") != profile.ClientID {
					t.Fatalf("Codex token body = %q", body)
				}
			} else {
				var values map[string]string
				if err := json.Unmarshal(body, &values); err != nil || values["grant_type"] != "authorization_code" || values["client_id"] != profile.ClientID || values["state"] != "state-1" {
					t.Fatalf("Claude token body = %q", body)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"access-1","refresh_token":"refresh-1","id_token":"id-1","expires_in":60,"scope":"user:profile user:inference"}`)
		}))
		profile.OAuthTokenURL = server.URL + "/oauth/token"
		profile.APIBaseURL = server.URL
		got, err := (HTTPClient{}).ExchangeCode(context.Background(), profile, "code-1", "verifier-1", "http://localhost/callback", "state-1")
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got.AccessToken != "access-1" || got.RefreshToken != "refresh-1" || len(got.Scopes) != 2 {
			t.Fatalf("token bundle = %#v", got)
		}
	}
}

func TestOAuthRefreshAndInferenceFixturesVerifyHeadersAndPaths(t *testing.T) {
	profiles := []EndpointProfile{CodexChatGPTProfile(), ClaudeConsoleOAuthProfile(), CodexAPIKeyProfile(), ClaudeAPIKeyProfile()}
	for _, original := range profiles {
		profile := original
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/oauth/token":
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				var values map[string]any
				if err := json.Unmarshal(body, &values); err != nil || values["grant_type"] != "refresh_token" || values["refresh_token"] != "refresh-1" || values["client_id"] != profile.ClientID {
					t.Fatalf("%s refresh body = %q", profile.Kind, body)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"access_token":"access-2","expires_in":60}`)
			case "/responses", "/v1/messages":
				if profile.Kind == KindClaude && profile.AuthMode == AuthModeAPIKey {
					if r.Header.Get("x-api-key") != "secret-1" || r.Header.Get("Authorization") != "" {
						t.Fatalf("Claude API key headers = %#v", r.Header)
					}
				} else if r.Header.Get("Authorization") != "Bearer secret-1" {
					t.Fatalf("OAuth/OpenAI headers = %#v", r.Header)
				}
				if profile.Kind == KindClaude && r.Header.Get("anthropic-version") != "2023-06-01" {
					t.Fatalf("Claude anthropic-version header = %q", r.Header.Get("anthropic-version"))
				}
				if profile.Kind == KindClaude && profile.AuthMode == AuthModeOAuth && r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
					t.Fatalf("Claude OAuth beta header = %q", r.Header.Get("anthropic-beta"))
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"ok":true}`)
			default:
				http.NotFound(w, r)
			}
		}))
		profile.OAuthTokenURL = server.URL + "/oauth/token"
		profile.APIBaseURL = server.URL
		if profile.AuthMode == AuthModeOAuth {
			got, err := (HTTPClient{}).Refresh(context.Background(), profile, contracts.TokenBundle{RefreshToken: "refresh-1", Scopes: []string{"user:inference"}})
			if err != nil || got.AccessToken != "access-2" || got.RefreshToken != "refresh-1" {
				t.Fatalf("refresh bundle=%#v err=%v", got, err)
			}
		}
		endpoint, err := profile.InferenceURL()
		if err != nil || !strings.HasSuffix(endpoint, profile.InferencePath) {
			t.Fatalf("inference endpoint=%q err=%v", endpoint, err)
		}
		response, err := (HTTPClient{}).Inference(context.Background(), profile, "secret-1", []byte(`{"model":"fixture"}`))
		server.Close()
		if err != nil || string(response) != `{"ok":true}` {
			t.Fatalf("inference response=%q err=%v", response, err)
		}
	}
}

func TestProfileValidationRejectsDynamicOrMalformedEndpoints(t *testing.T) {
	profile := CodexChatGPTProfile()
	profile.APIBaseURL = "file:///tmp/provider"
	if _, err := profile.InferenceURL(); err == nil {
		t.Fatal("non-HTTP profile URL was accepted")
	}
	profile = ClaudeAPIKeyProfile()
	if _, err := profile.InferenceHeaders(""); err == nil {
		t.Fatal("empty access token was accepted")
	}
}

func TestOfficialProfilesKeepVerifiedInferencePaths(t *testing.T) {
	cases := []struct {
		name      string
		profile   EndpointProfile
		inference string
		authorize string
		token     string
		clientID  string
	}{
		{
			name:      "codex chatgpt oauth",
			profile:   CodexChatGPTProfile(),
			inference: "https://chatgpt.com/backend-api/codex/responses",
			authorize: "https://auth.openai.com/oauth/authorize",
			token:     "https://auth.openai.com/oauth/token",
			clientID:  "app_EMoamEEZ73f0CkXaXp7hrann",
		},
		{
			name:      "codex api key",
			profile:   CodexAPIKeyProfile(),
			inference: "https://api.openai.com/v1/responses",
		},
		{
			name:      "claude console oauth",
			profile:   ClaudeConsoleOAuthProfile(),
			inference: "https://api.anthropic.com/v1/messages",
			authorize: "https://platform.claude.com/oauth/authorize",
			token:     "https://platform.claude.com/v1/oauth/token",
			clientID:  "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		},
		{
			name:      "claude api key",
			profile:   ClaudeAPIKeyProfile(),
			inference: "https://api.anthropic.com/v1/messages",
		},
		{
			name:      "grok api key",
			profile:   GrokAPIKeyProfile(),
			inference: "https://api.x.ai/v1/chat/completions",
		},
		{
			name:      "github copilot oauth",
			profile:   GitHubCopilotProfile(),
			inference: "https://api.githubcopilot.com/chat/completions",
			authorize: "https://github.com/login/device/code",
			token:     "https://api.github.com/copilot_internal/v2/token",
			clientID:  "Iv1.b507a08c87ecfe98",
		},
		{
			name:      "antigravity oauth",
			profile:   AntigravityOAuthProfile(),
			inference: "https://cloudcode-pa.googleapis.com/v1internal:generateContent",
			authorize: "https://accounts.google.com/o/oauth2/v2/auth",
			token:     "https://oauth2.googleapis.com/token",
			clientID:  "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com",
		},
		{
			name:      "kiro imported oauth",
			profile:   KiroOAuthProfile(),
			inference: "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.profile.Validate(); err != nil {
				t.Fatal(err)
			}
			inference, err := tc.profile.InferenceURL()
			if err != nil || inference != tc.inference {
				t.Fatalf("inference=%q err=%v", inference, err)
			}
			if tc.authorize != "" && (tc.profile.OAuthAuthorizeURL != tc.authorize || tc.profile.OAuthTokenURL != tc.token || tc.profile.ClientID != tc.clientID) {
				t.Fatalf("OAuth profile endpoints: authorize=%q token=%q client_id=%q", tc.profile.OAuthAuthorizeURL, tc.profile.OAuthTokenURL, tc.profile.ClientID)
			}
		})
	}
}

func TestRefreshPreservesOmittedFieldsAndReadsJWTExpiry(t *testing.T) {
	expiresAt := time.Unix(1_900_000_000, 0).UTC()
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1900000000}`))
	accessToken := "header." + payload + ".signature"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("content type = %q", r.Header.Get("Content-Type"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"`+accessToken+`"}`)
	}))
	defer server.Close()
	profile := CodexChatGPTProfile()
	profile.OAuthTokenURL = server.URL
	profile.APIBaseURL = server.URL
	current := contracts.TokenBundle{RefreshToken: "refresh-1", IDToken: "id-1", Scopes: []string{"openid"}, AccountID: "account-1", Email: "user@example.test"}
	got, err := (HTTPClient{}).Refresh(context.Background(), profile, current)
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshToken != current.RefreshToken || got.IDToken != current.IDToken || got.AccountID != current.AccountID || got.Email != current.Email || len(got.Scopes) != 1 || !got.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("preserved bundle = %#v", got)
	}
}

func TestHTTPErrorClassifiesWithoutLeakingResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","detail":"secret-response"}`)
	}))
	defer server.Close()
	profile := CodexChatGPTProfile()
	profile.OAuthTokenURL = server.URL
	profile.APIBaseURL = server.URL
	_, err := (HTTPClient{}).Refresh(context.Background(), profile, contracts.TokenBundle{RefreshToken: "refresh-1"})
	var upstream *HTTPError
	if !errors.As(err, &upstream) || ErrorClass(err) != contracts.ErrorAuthInvalid || upstream.Code != "invalid_grant" {
		t.Fatalf("error=%#v class=%s", err, ErrorClass(err))
	}
	if strings.Contains(err.Error(), "secret-response") || strings.Contains(err.Error(), "refresh-1") {
		t.Fatalf("sensitive data leaked in error: %v", err)
	}
}

func TestCodexRevokeUsesJSONTokenTypeHint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("content type = %q", r.Header.Get("Content-Type"))
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["token"] != "refresh-1" || payload["token_type_hint"] != "refresh_token" || payload["client_id"] != CodexChatGPTProfile().ClientID {
			t.Fatalf("revoke payload = %#v", payload)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	profile := CodexChatGPTProfile()
	profile.OAuthRevokeURL = server.URL
	profile.APIBaseURL = server.URL
	if err := (HTTPClient{}).Revoke(context.Background(), profile, "refresh-1", "refresh_token"); err != nil {
		t.Fatal(err)
	}
}

func TestProviderResponseSizeIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(make([]byte, maxOAuthResponseBytes+1))
	}))
	defer server.Close()
	profile := CodexChatGPTProfile()
	profile.OAuthTokenURL = server.URL
	profile.APIBaseURL = server.URL
	if _, err := (HTTPClient{}).Refresh(context.Background(), profile, contracts.TokenBundle{RefreshToken: "refresh-1"}); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("got %v, want size limit error", err)
	}
}
