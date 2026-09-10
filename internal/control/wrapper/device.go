package wrapper

// Device authorization grant (RFC 8628) executor. This is the second shape of
// interactive authorization the wrapper has to run: unlike PKCE there is no
// loopback callback, so the worker cannot learn when the user finished -- it
// polls the token endpoint until the authorization server changes its answer.
//
// Everything here that is protocol-defined (the grant type URN, the four
// polling error codes, the 5-second slow_down increment) comes from RFC 8628
// §3.4/§3.5. Everything that is provider-specific stays out of this file: see
// defaultDeviceProfile for why no provider currently resolves to a complete
// profile, and docs/E2-WRAPPER-EXECUTORS.md for what C4 has to supply.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

const (
	ExecutorDevice = "device"

	// deviceAuthMode is the auth_mode carried in the encrypted job input. It
	// is what routes a job to this executor rather than to PKCEExecutor.
	deviceAuthMode = "device"

	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

	// RFC 8628 §3.5: if the server has no interval, poll every 5 seconds, and
	// on slow_down increase the interval by 5 seconds.
	defaultDevicePollInterval = 5 * time.Second
	defaultSlowDownIncrement  = 5 * time.Second
	defaultDeviceTimeout      = 5 * time.Minute

	maxDeviceResponseBytes = 1 << 20

	ErrorDeviceProfilePending = "device_profile_pending"
	ErrorDeviceCodeRequest    = "device_code_request_failed"
	ErrorDeviceDenied         = "device_denied"
	ErrorDeviceExpired        = "device_code_expired"
	ErrorDeviceTokenInvalid   = "device_token_invalid"
	ErrorDeviceTimeout        = "device_authorization_timeout"
)

// DeviceProfile is the endpoint set a device flow needs.
//
// It is deliberately NOT providerapi.EndpointProfile. That struct has one
// OAuthTokenURL, and for the providers that use device flow the device-code
// endpoint and the device access-token endpoint are two different hosts from
// the endpoint the provider itself refreshes against. Widening the shared
// profile would mean writing an upstream endpoint into a contract that no
// evidence in this repository supports; keeping a local struct means the gap
// is visible instead.
type DeviceProfile struct {
	// DeviceCodeURL is where the worker requests a device_code / user_code pair.
	DeviceCodeURL string
	// TokenURL is where the worker polls for the grant. It is a distinct
	// endpoint from DeviceCodeURL and from the provider's own refresh URL.
	TokenURL string
	ClientID string
	Scope    string
}

func (p DeviceProfile) Validate() error {
	if strings.TrimSpace(p.DeviceCodeURL) == "" {
		return errors.New("device profile has no device-code endpoint")
	}
	if strings.TrimSpace(p.TokenURL) == "" {
		return errors.New("device profile has no device token endpoint")
	}
	if strings.TrimSpace(p.ClientID) == "" {
		return errors.New("device profile has no client id")
	}
	for name, raw := range map[string]string{"device-code": p.DeviceCodeURL, "token": p.TokenURL} {
		parsed, err := url.Parse(raw)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("device profile %s endpoint is not an http URL", name)
		}
	}
	return nil
}

// DeviceInstruction is what a human has to be shown for the flow to proceed.
// It is passed to Prompt rather than logged from inside authorize so a caller
// can route it somewhere a person will actually see.
type DeviceInstruction struct {
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               time.Duration
}

type DeviceExecutor struct {
	Cipher     *EnvelopeCipher
	HTTPClient *http.Client
	Profile    func(string) (DeviceProfile, bool)
	Prompt     func(context.Context, DeviceInstruction) error

	// PollInterval overrides the interval the authorization server asks for.
	// Production leaves it zero and honours the server; the tests set it to a
	// few milliseconds so a poll loop finishes inside a unit test.
	PollInterval time.Duration
	// SlowDownIncrement defaults to RFC 8628's 5 seconds.
	SlowDownIncrement time.Duration
	// Timeout caps the whole flow. The server's expires_in still applies and
	// whichever is shorter wins.
	Timeout time.Duration
}

func (e DeviceExecutor) Execute(ctx context.Context, job contracts.WrapperJob) ([]byte, error) {
	if err := job.Validate(); err != nil {
		return nil, executionError(ErrorUnsupportedJob, false, err)
	}
	if job.Operation != contracts.WrapperAuthorize {
		return nil, executionError(ErrorUnsupportedJob, false, contracts.ErrUnsupportedCapability)
	}
	if e.Cipher == nil {
		return nil, executionError(ErrorInvalidEnvelope, false, errors.New("wrapper envelope cipher is not configured"))
	}
	plaintext, err := e.Cipher.Decrypt(job.JobID, job.EncryptedInput)
	if err != nil {
		return nil, executionError(ErrorInvalidEnvelope, false, err)
	}
	var input authorizeInput
	if err := json.Unmarshal(plaintext, &input); err != nil || input.AuthMode != deviceAuthMode {
		return nil, executionError(ErrorInvalidEnvelope, false, errors.New("wrapper device authorize input is invalid"))
	}
	resolver := e.Profile
	if resolver == nil {
		resolver = defaultDeviceProfile
	}
	profile, ok := resolver(job.Provider)
	if !ok {
		return nil, executionError(ErrorUnsupportedJob, false, contracts.ErrUnsupportedCapability)
	}
	if err := profile.Validate(); err != nil {
		// Not retryable: an incomplete profile is a missing fact about the
		// upstream, and retrying cannot supply it.
		return nil, executionError(ErrorDeviceProfilePending, false, err)
	}
	bundle, err := e.authorize(ctx, profile, input.Proxy)
	if err != nil {
		return nil, err
	}
	output, err := json.Marshal(authorizeOutput{TokenBundle: bundle})
	if err != nil {
		return nil, executionError(ErrorInvalidEnvelope, false, err)
	}
	encrypted, err := e.Cipher.Encrypt(job.JobID, output)
	if err != nil {
		return nil, executionError(ErrorInvalidEnvelope, false, err)
	}
	return encrypted, nil
}

type deviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
	Error                   string `json:"error"`
}

type deviceTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
}

func (e DeviceExecutor) authorize(ctx context.Context, profile DeviceProfile, proxy string) (contracts.TokenBundle, error) {
	client, err := e.client(proxy)
	if err != nil {
		return contracts.TokenBundle{}, executionError(ErrorDeviceCodeRequest, false, err)
	}

	start, err := e.requestDeviceCode(ctx, client, profile)
	if err != nil {
		return contracts.TokenBundle{}, err
	}

	instruction := DeviceInstruction{
		UserCode:                start.UserCode,
		VerificationURI:         start.VerificationURI,
		VerificationURIComplete: start.VerificationURIComplete,
		ExpiresIn:               time.Duration(start.ExpiresIn) * time.Second,
	}
	prompt := e.Prompt
	if prompt == nil {
		prompt = logDeviceInstruction
	}
	if err := prompt(ctx, instruction); err != nil {
		return contracts.TokenBundle{}, executionError(ErrorDeviceCodeRequest, false, err)
	}

	return e.poll(ctx, client, profile, start)
}

func (e DeviceExecutor) requestDeviceCode(ctx context.Context, client *http.Client, profile DeviceProfile) (deviceCodeResponse, error) {
	fields := url.Values{"client_id": {profile.ClientID}}
	if strings.TrimSpace(profile.Scope) != "" {
		fields.Set("scope", profile.Scope)
	}
	body, status, err := postForm(ctx, client, profile.DeviceCodeURL, fields)
	if err != nil {
		// Transport failures are worth another attempt; nothing has been shown
		// to a human yet, so a retry costs nothing.
		return deviceCodeResponse{}, executionError(ErrorDeviceCodeRequest, true, err)
	}
	var parsed deviceCodeResponse
	if jsonErr := json.Unmarshal(body, &parsed); jsonErr != nil {
		return deviceCodeResponse{}, executionError(ErrorDeviceCodeRequest, false, fmt.Errorf("decode device code response: %w", jsonErr))
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices || parsed.Error != "" {
		return deviceCodeResponse{}, executionError(ErrorDeviceCodeRequest, status >= http.StatusInternalServerError,
			fmt.Errorf("device code request failed with HTTP %d (%s)", status, parsed.Error))
	}
	if strings.TrimSpace(parsed.DeviceCode) == "" || strings.TrimSpace(parsed.UserCode) == "" || strings.TrimSpace(parsed.VerificationURI) == "" {
		return deviceCodeResponse{}, executionError(ErrorDeviceCodeRequest, false, errors.New("device code response is missing required fields"))
	}
	return parsed, nil
}

func (e DeviceExecutor) poll(ctx context.Context, client *http.Client, profile DeviceProfile, start deviceCodeResponse) (contracts.TokenBundle, error) {
	interval := e.PollInterval
	if interval <= 0 {
		interval = time.Duration(start.Interval) * time.Second
		if interval <= 0 {
			interval = defaultDevicePollInterval
		}
	}
	increment := e.SlowDownIncrement
	if increment <= 0 {
		increment = defaultSlowDownIncrement
	}

	deadline := e.deadline(start)
	pollCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	fields := url.Values{
		"client_id":   {profile.ClientID},
		"device_code": {start.DeviceCode},
		"grant_type":  {deviceGrantType},
	}
	for {
		body, status, err := postForm(pollCtx, client, profile.TokenURL, fields)
		if err != nil {
			if pollCtx.Err() != nil {
				return contracts.TokenBundle{}, e.timeoutError(ctx, pollCtx)
			}
			return contracts.TokenBundle{}, executionError(ErrorDeviceTokenInvalid, true, err)
		}
		var parsed deviceTokenResponse
		if jsonErr := json.Unmarshal(body, &parsed); jsonErr != nil {
			return contracts.TokenBundle{}, executionError(ErrorDeviceTokenInvalid, false, fmt.Errorf("decode device token response: %w", jsonErr))
		}

		// RFC 8628 §3.5 puts the pending/slow_down signal in the error field of
		// an otherwise well-formed response, so the error field is checked
		// before the status code -- a 400 carrying authorization_pending is the
		// normal case, not a failure.
		switch parsed.Error {
		case "":
		case "authorization_pending":
			if err := waitFor(pollCtx, interval); err != nil {
				return contracts.TokenBundle{}, e.timeoutError(ctx, pollCtx)
			}
			continue
		case "slow_down":
			interval += increment
			if err := waitFor(pollCtx, interval); err != nil {
				return contracts.TokenBundle{}, e.timeoutError(ctx, pollCtx)
			}
			continue
		case "access_denied":
			// The user said no. Retrying would re-prompt them for the same
			// answer, so this is terminal.
			return contracts.TokenBundle{}, executionError(ErrorDeviceDenied, false, errors.New("device authorization was denied"))
		case "expired_token":
			// The code aged out. A fresh job gets a fresh code, so this is
			// retryable at the job level even though this attempt is over.
			return contracts.TokenBundle{}, executionError(ErrorDeviceExpired, true, errors.New("device code expired before authorization"))
		default:
			return contracts.TokenBundle{}, executionError(ErrorDeviceTokenInvalid, false,
				fmt.Errorf("device token endpoint returned %q", parsed.Error))
		}

		if status < http.StatusOK || status >= http.StatusMultipleChoices {
			return contracts.TokenBundle{}, executionError(ErrorDeviceTokenInvalid, status >= http.StatusInternalServerError,
				fmt.Errorf("device token request failed with HTTP %d", status))
		}
		if strings.TrimSpace(parsed.AccessToken) == "" {
			return contracts.TokenBundle{}, executionError(ErrorDeviceTokenInvalid, false, errors.New("device token response has no access_token"))
		}
		return deviceTokenBundle(parsed), nil
	}
}

// timeoutError distinguishes "the caller gave up" from "the flow ran out of
// time". Only the latter is a device-flow outcome worth its own code.
func (e DeviceExecutor) timeoutError(ctx, pollCtx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return executionError(ErrorDeviceTimeout, true, pollCtx.Err())
}

func (e DeviceExecutor) deadline(start deviceCodeResponse) time.Time {
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = defaultDeviceTimeout
	}
	if start.ExpiresIn > 0 {
		if expiry := time.Duration(start.ExpiresIn) * time.Second; expiry < timeout {
			timeout = expiry
		}
	}
	return time.Now().Add(timeout)
}

func (e DeviceExecutor) client(proxy string) (*http.Client, error) {
	base := e.HTTPClient
	if base == nil {
		base = &http.Client{Timeout: 30 * time.Second}
	}
	return (providerapi.HTTPClient{Client: base}).ClientForProxy(proxy)
}

func deviceTokenBundle(parsed deviceTokenResponse) contracts.TokenBundle {
	bundle := contracts.TokenBundle{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		IDToken:      parsed.IDToken,
	}
	if parsed.ExpiresIn > 0 {
		bundle.ExpiresAt = time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second).UTC()
	}
	if scope := strings.TrimSpace(parsed.Scope); scope != "" {
		bundle.Scopes = strings.Fields(scope)
	}
	return bundle
}

func postForm(ctx context.Context, client *http.Client, endpoint string, fields url.Values) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(fields.Encode()))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxDeviceResponseBytes+1))
	if err != nil {
		return nil, response.StatusCode, err
	}
	if len(body) > maxDeviceResponseBytes {
		return nil, response.StatusCode, fmt.Errorf("device flow response exceeds %d bytes", maxDeviceResponseBytes)
	}
	return body, response.StatusCode, nil
}

// logDeviceInstruction is the default prompt. It logs the user code and the
// verification URI and additionally tries to open a browser; the browser is
// best-effort because, unlike PKCE, the code still has to be typed by hand and
// the log line is the part that must not be lost.
func logDeviceInstruction(ctx context.Context, instruction DeviceInstruction) error {
	target := instruction.VerificationURIComplete
	if target == "" {
		target = instruction.VerificationURI
	}
	log.Printf("wrapper device authorization: open %s and enter code %s (expires in %s)",
		instruction.VerificationURI, instruction.UserCode, instruction.ExpiresIn)
	_ = openBrowser(ctx, target)
	return nil
}

// defaultDeviceProfile resolves a provider to its device endpoints.
//
// GitHub Copilot is the only provider in this repository that uses device
// flow. Its device-code endpoint and client id are established in-repo
// (provider.GitHubCopilotProfile, whose OAuthTokenURL is the Copilot API token
// endpoint, NOT the device access-token endpoint). The device access-token
// endpoint is not in this repository; the rewrite blueprint named in 总览 §9
// records it (elucid-relay apps/oauth-wrapper/src/strategies.mjs deviceDefaults,
// "https://github.com/login/oauth/access_token").
//
// TokenURL is nonetheless left empty on purpose. Polling that endpoint yields a
// GitHub OAuth token, which is not a Copilot API credential: the blueprint
// follows it with a second exchange (githubCopilotBundleFromGitHubToken, via
// OAuthTokenURL) that this executor does not implement. Wiring the URL in would
// make the job COMPLETE with a token that cannot serve traffic -- a silent
// success, the exact failure mode this round set out to remove from the fixture
// path. So Execute fails with device_profile_pending instead: explicit and
// non-retryable, naming what is missing. C4 supplies the endpoint AND the
// exchange together. See docs/E2-WRAPPER-EXECUTORS.md.
func defaultDeviceProfile(providerName string) (DeviceProfile, bool) {
	if providerName != providerapi.KindCopilot {
		return DeviceProfile{}, false
	}
	profile := providerapi.GitHubCopilotProfile()
	return DeviceProfile{
		DeviceCodeURL: profile.OAuthAuthorizeURL,
		TokenURL:      "",
		ClientID:      profile.ClientID,
	}, true
}
