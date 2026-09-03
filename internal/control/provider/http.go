package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

type HTTPClient struct {
	Client *http.Client
	Now    func() time.Time
}

var defaultHTTPClient = &http.Client{Timeout: 30 * time.Second}

const (
	maxOAuthResponseBytes = 1 << 20
	maxInferenceBytes     = 16 << 20
	maxErrorResponseBytes = 64 << 10
)

type HTTPError struct {
	Operation  string
	StatusCode int
	Code       string
	Class      contracts.ErrorClass
}

func (e *HTTPError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s failed with HTTP %d (%s)", e.Operation, e.StatusCode, e.Code)
	}
	return fmt.Sprintf("%s failed with HTTP %d", e.Operation, e.StatusCode)
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

func (c HTTPClient) ExchangeCode(ctx context.Context, profile EndpointProfile, code, verifier, redirectURI, state string) (contracts.TokenBundle, error) {
	if err := validateOAuthRequest(profile, code, verifier, redirectURI); err != nil {
		return contracts.TokenBundle{}, err
	}
	if len(state) > 4096 {
		return contracts.TokenBundle{}, fmt.Errorf("%w: state is too large", contracts.ErrInvalidContract)
	}
	fields := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {profile.ClientID},
		"code_verifier": {verifier},
	}
	var body []byte
	contentType := "application/x-www-form-urlencoded"
	var err error
	if profile.Kind == KindClaude {
		contentType = "application/json"
		payload := map[string]string{
			"grant_type":    "authorization_code",
			"code":          code,
			"redirect_uri":  redirectURI,
			"client_id":     profile.ClientID,
			"code_verifier": verifier,
		}
		if state != "" {
			payload["state"] = state
		}
		body, err = json.Marshal(payload)
	} else {
		body = []byte(fields.Encode())
	}
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	return c.doTokenRequest(ctx, profile, contentType, body, contracts.TokenBundle{})
}

func (c HTTPClient) Refresh(ctx context.Context, profile EndpointProfile, current contracts.TokenBundle) (contracts.TokenBundle, error) {
	if err := profile.Validate(); err != nil {
		return contracts.TokenBundle{}, err
	}
	if profile.AuthMode != AuthModeOAuth || strings.TrimSpace(current.RefreshToken) == "" {
		return contracts.TokenBundle{}, fmt.Errorf("%w: OAuth refresh token is missing", contracts.ErrInvalidContract)
	}
	var body []byte
	contentType := "application/json"
	var err error
	if profile.Kind == KindClaude {
		payload := map[string]any{
			"grant_type":    "refresh_token",
			"refresh_token": current.RefreshToken,
			"client_id":     profile.ClientID,
		}
		if len(current.Scopes) > 0 {
			payload["scope"] = strings.Join(current.Scopes, " ")
		}
		body, err = json.Marshal(payload)
	} else {
		body, err = json.Marshal(map[string]string{
			"grant_type":    "refresh_token",
			"refresh_token": current.RefreshToken,
			"client_id":     profile.ClientID,
		})
	}
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	return c.doTokenRequest(ctx, profile, contentType, body, current)
}

func (c HTTPClient) Inference(ctx context.Context, profile EndpointProfile, accessToken string, payload []byte) ([]byte, error) {
	endpoint, err := profile.InferenceURL()
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("%w: inference payload is empty", contracts.ErrInvalidContract)
	}
	headers, err := profile.InferenceHeaders(accessToken)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	responseBody, err := readLimited(resp.Body, maxInferenceBytes)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, newHTTPError("inference", resp.StatusCode, responseBody)
	}
	return responseBody, nil
}

func (c HTTPClient) Revoke(ctx context.Context, profile EndpointProfile, token, tokenType string) error {
	if err := profile.Validate(); err != nil {
		return err
	}
	if profile.AuthMode != AuthModeOAuth || profile.OAuthRevokeURL == "" {
		return contracts.ErrUnsupportedCapability
	}
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("%w: revoke token is empty", contracts.ErrInvalidContract)
	}
	if tokenType != "access_token" && tokenType != "refresh_token" {
		return fmt.Errorf("%w: invalid revoke token type", contracts.ErrInvalidContract)
	}
	payload := map[string]string{"token": token, "token_type_hint": tokenType}
	if tokenType == "refresh_token" {
		payload["client_id"] = profile.ClientID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, profile.OAuthRevokeURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	responseBody, err := readLimited(resp.Body, maxErrorResponseBytes)
	if err != nil {
		return err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return newHTTPError("OAuth revoke", resp.StatusCode, responseBody)
	}
	return nil
}

func (c HTTPClient) doTokenRequest(ctx context.Context, profile EndpointProfile, contentType string, body []byte, fallback contracts.TokenBundle) (contracts.TokenBundle, error) {
	if err := profile.Validate(); err != nil {
		return contracts.TokenBundle{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, profile.OAuthTokenURL, bytes.NewReader(body))
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		responseBody, readErr := readLimited(resp.Body, maxErrorResponseBytes)
		if readErr != nil {
			return contracts.TokenBundle{}, readErr
		}
		return contracts.TokenBundle{}, newHTTPError("OAuth token", resp.StatusCode, responseBody)
	}
	responseBody, err := readLimited(resp.Body, maxOAuthResponseBytes)
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	var token tokenResponse
	if err := json.Unmarshal(responseBody, &token); err != nil {
		return contracts.TokenBundle{}, fmt.Errorf("decode OAuth response: %w", err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return contracts.TokenBundle{}, fmt.Errorf("%w: OAuth response has no access_token", contracts.ErrInvalidContract)
	}
	if token.RefreshToken == "" {
		token.RefreshToken = fallback.RefreshToken
	}
	if token.IDToken == "" {
		token.IDToken = fallback.IDToken
	}
	bundle := contracts.TokenBundle{
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		IDToken:      token.IDToken,
		AccountID:    fallback.AccountID,
		Email:        fallback.Email,
	}
	if token.Scope == "" {
		bundle.Scopes = append([]string(nil), fallback.Scopes...)
	} else {
		bundle.Scopes = strings.Fields(token.Scope)
	}
	if token.ExpiresIn > 0 {
		bundle.ExpiresAt = c.now().Add(time.Duration(token.ExpiresIn) * time.Second)
	} else {
		bundle.ExpiresAt = jwtExpiry(token.AccessToken)
	}
	return bundle, nil
}

func (c HTTPClient) httpClient() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return defaultHTTPClient
}

func (c HTTPClient) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func newHTTPError(operation string, statusCode int, body []byte) error {
	code := oauthErrorCode(body)
	class := contracts.ErrorForbiddenCapability
	switch {
	case code == "invalid_grant" || code == "invalid_token" || statusCode == http.StatusUnauthorized:
		class = contracts.ErrorAuthInvalid
	case statusCode == http.StatusForbidden:
		class = contracts.ErrorForbiddenTransport
	case statusCode == http.StatusTooManyRequests:
		class = contracts.ErrorRateLimitedKnown
	case statusCode >= http.StatusInternalServerError:
		class = contracts.ErrorUpstream5xx
	}
	return &HTTPError{Operation: operation, StatusCode: statusCode, Code: code, Class: class}
}

func oauthErrorCode(body []byte) string {
	var payload struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.Error) == 0 {
		return ""
	}
	var code string
	if json.Unmarshal(payload.Error, &code) == nil {
		return code
	}
	var nested struct {
		Code string `json:"code"`
		Type string `json:"type"`
	}
	if json.Unmarshal(payload.Error, &nested) != nil {
		return ""
	}
	if nested.Code != "" {
		return nested.Code
	}
	return nested.Type
}

func ErrorClass(err error) contracts.ErrorClass {
	var upstream *HTTPError
	if errors.As(err, &upstream) {
		return upstream.Class
	}
	return contracts.ErrorNetwork
}

func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		ExpiresAt json.Number `json:"exp"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if decoder.Decode(&claims) != nil {
		return time.Time{}
	}
	seconds, err := claims.ExpiresAt.Int64()
	if err != nil || seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}

func validateOAuthRequest(profile EndpointProfile, code, verifier, redirectURI string) error {
	if err := profile.Validate(); err != nil {
		return err
	}
	if profile.AuthMode != AuthModeOAuth {
		return fmt.Errorf("%w: API key profile has no OAuth code exchange", ErrInvalidProfile)
	}
	for name, value := range map[string]string{"code": code, "code_verifier": verifier, "redirect_uri": redirectURI} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: %s is empty", contracts.ErrInvalidContract, name)
		}
	}
	if len(code) > 16<<10 || len(verifier) > 1024 || len(redirectURI) > 2048 {
		return fmt.Errorf("%w: OAuth request field is too large", contracts.ErrInvalidContract)
	}
	if _, err := parseHTTPURL(redirectURI); err != nil {
		return fmt.Errorf("%w: invalid redirect_uri", contracts.ErrInvalidContract)
	}
	return nil
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("provider response exceeds size limit")
	}
	return body, nil
}
