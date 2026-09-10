package wrapper

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/elucid/gateway-platform/internal/control/credentials"
	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

const (
	ExecutorOAuth   = "oauth"
	ExecutorFixture = "fixture"

	defaultAuthorizePollInterval = 100 * time.Millisecond
	defaultCallbackTimeout       = 5 * time.Minute
	codexOAuthScope              = "openid profile email offline_access api.connectors.read api.connectors.invoke"

	ErrorInvalidEnvelope = "invalid_envelope"
	ErrorUnsupportedJob  = "unsupported_job"
	ErrorCallbackListen  = "callback_listen_failed"
	ErrorBrowserOpen     = "browser_open_failed"
	ErrorCallbackDenied  = "oauth_denied"
	ErrorCallbackInvalid = "oauth_callback_invalid"
	ErrorCallbackTimeout = "oauth_callback_timeout"
	ErrorTokenExchange   = "oauth_exchange_failed"
)

type EnvelopeCipher struct {
	cipher *credentials.Cipher
}

func NewEnvelopeCipher(encodedKey string) (*EnvelopeCipher, error) {
	if strings.TrimSpace(encodedKey) == "" {
		return nil, errors.New("wrapper envelope key is required")
	}
	key, err := base64.StdEncoding.Strict().DecodeString(encodedKey)
	if err != nil {
		return nil, errors.New("wrapper envelope key must be valid base64")
	}
	cipher, err := credentials.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("wrapper envelope key: %w", err)
	}
	return &EnvelopeCipher{cipher: cipher}, nil
}

func (c *EnvelopeCipher) Encrypt(jobID string, plaintext []byte) ([]byte, error) {
	if c == nil || c.cipher == nil {
		return nil, errors.New("wrapper envelope cipher is not configured")
	}
	return c.cipher.Encrypt(jobID, plaintext)
}

func (c *EnvelopeCipher) Decrypt(jobID string, ciphertext []byte) ([]byte, error) {
	if c == nil || c.cipher == nil {
		return nil, errors.New("wrapper envelope cipher is not configured")
	}
	return c.cipher.Decrypt(jobID, ciphertext)
}

type authorizeInput struct {
	AuthMode string `json:"auth_mode"`
	Proxy    string `json:"proxy,omitempty"`
}

type authorizeOutput struct {
	TokenBundle contracts.TokenBundle `json:"token_bundle"`
}

type JobFailureError struct {
	Code      string
	Retryable bool
}

func (e *JobFailureError) Error() string {
	return fmt.Sprintf("wrapper authorize failed: code=%s retryable=%t", e.Code, e.Retryable)
}

type AuthorizeClient struct {
	Queue        Queue
	Cipher       *EnvelopeCipher
	PollInterval time.Duration
}

// Authorize is the control-side producer for interactive OAuth jobs.
func (c AuthorizeClient) Authorize(ctx context.Context, providerName string) (contracts.TokenBundle, error) {
	return c.AuthorizeWithProxy(ctx, providerName, "")
}

func (c AuthorizeClient) AuthorizeWithProxy(ctx context.Context, providerName, proxy string) (contracts.TokenBundle, error) {
	return c.authorize(ctx, providerName, proxy, "pkce")
}

// AuthorizeDevice enqueues the device-authorization variant. The producer side
// is identical to PKCE -- same queue, same envelope, same terminal contract --
// and only the auth_mode inside the encrypted input differs, which is what
// routes the job to DeviceExecutor on the worker.
func (c AuthorizeClient) AuthorizeDevice(ctx context.Context, providerName, proxy string) (contracts.TokenBundle, error) {
	return c.authorize(ctx, providerName, proxy, deviceAuthMode)
}

func (c AuthorizeClient) authorize(ctx context.Context, providerName, proxy, authMode string) (contracts.TokenBundle, error) {
	if c.Queue.Redis == nil || c.Cipher == nil {
		return contracts.TokenBundle{}, errors.New("wrapper authorize client is not configured")
	}
	if strings.TrimSpace(providerName) == "" {
		return contracts.TokenBundle{}, fmt.Errorf("%w: wrapper provider is empty", contracts.ErrInvalidContract)
	}
	jobID, err := newJobID()
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	plaintext, err := json.Marshal(authorizeInput{AuthMode: authMode, Proxy: strings.TrimSpace(proxy)})
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	encrypted, err := c.Cipher.Encrypt(jobID, plaintext)
	if err != nil {
		return contracts.TokenBundle{}, err
	}
	job := contracts.WrapperJob{
		SchemaVersion:  contracts.SchemaVersion,
		JobID:          jobID,
		Provider:       providerName,
		Operation:      contracts.WrapperAuthorize,
		EncryptedInput: encrypted,
	}
	if _, err := c.Queue.Enqueue(ctx, job); err != nil {
		return contracts.TokenBundle{}, fmt.Errorf("enqueue wrapper authorize job: %w", err)
	}
	pollInterval := c.PollInterval
	if pollInterval <= 0 {
		pollInterval = defaultAuthorizePollInterval
	}
	for {
		raw, err := c.Queue.Terminal(ctx, jobID)
		if errors.Is(err, redis.Nil) {
			if err := waitFor(ctx, pollInterval); err != nil {
				return contracts.TokenBundle{}, err
			}
			continue
		}
		if err != nil {
			return contracts.TokenBundle{}, fmt.Errorf("read wrapper authorize result: %w", err)
		}
		var terminal terminalEnvelope
		if err := json.Unmarshal(raw, &terminal); err != nil {
			return contracts.TokenBundle{}, fmt.Errorf("decode wrapper authorize result: %w", err)
		}
		switch terminal.Status {
		case "failed":
			if terminal.Failure == nil || terminal.Failure.JobID != jobID || terminal.Failure.Validate() != nil {
				return contracts.TokenBundle{}, errors.New("wrapper authorize returned an invalid failure")
			}
			return contracts.TokenBundle{}, &JobFailureError{Code: terminal.Failure.Code, Retryable: terminal.Failure.Retryable}
		case "completed":
			if terminal.Completion == nil || terminal.Completion.JobID != jobID || terminal.Completion.Validate() != nil {
				return contracts.TokenBundle{}, errors.New("wrapper authorize returned an invalid completion")
			}
			plaintext, err := c.Cipher.Decrypt(jobID, terminal.Completion.EncryptedOutput)
			if err != nil {
				return contracts.TokenBundle{}, fmt.Errorf("decrypt wrapper authorize result: %w", err)
			}
			var output authorizeOutput
			if err := json.Unmarshal(plaintext, &output); err != nil {
				return contracts.TokenBundle{}, fmt.Errorf("decode wrapper authorize output: %w", err)
			}
			if strings.TrimSpace(output.TokenBundle.AccessToken) == "" && strings.TrimSpace(output.TokenBundle.RefreshToken) == "" {
				return contracts.TokenBundle{}, errors.New("wrapper authorize returned an empty token bundle")
			}
			return output.TokenBundle, nil
		default:
			return contracts.TokenBundle{}, errors.New("wrapper authorize returned an invalid terminal status")
		}
	}
}

type ExecutionError struct {
	Code      string
	Retryable bool
	Err       error
}

func (e *ExecutionError) Error() string {
	if e.Err == nil {
		return e.Code
	}
	return fmt.Sprintf("%s: %v", e.Code, e.Err)
}

func (e *ExecutionError) Unwrap() error { return e.Err }

type PKCEExecutor struct {
	Cipher          *EnvelopeCipher
	HTTPClient      *http.Client
	OpenURL         func(context.Context, string) error
	CallbackTimeout time.Duration
	Profile         func(string) (providerapi.EndpointProfile, bool)
}

func (e PKCEExecutor) Execute(ctx context.Context, job contracts.WrapperJob) ([]byte, error) {
	if err := job.Validate(); err != nil {
		return nil, executionError(ErrorUnsupportedJob, false, err)
	}
	if job.Operation != contracts.WrapperAuthorize || job.Provider != providerapi.KindCodex {
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
	if err := json.Unmarshal(plaintext, &input); err != nil || input.AuthMode != "pkce" {
		return nil, executionError(ErrorInvalidEnvelope, false, errors.New("wrapper authorize input is invalid"))
	}
	profileResolver := e.Profile
	if profileResolver == nil {
		profileResolver = defaultOAuthProfile
	}
	profile, ok := profileResolver(job.Provider)
	if !ok || profile.Kind != job.Provider || profile.AuthMode != providerapi.AuthModeOAuth {
		return nil, executionError(ErrorUnsupportedJob, false, contracts.ErrUnsupportedCapability)
	}
	if err := profile.Validate(); err != nil {
		return nil, executionError(ErrorUnsupportedJob, false, err)
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

type callbackResult struct {
	code string
	err  error
}

func (e PKCEExecutor) authorize(ctx context.Context, profile providerapi.EndpointProfile, proxy string) (contracts.TokenBundle, error) {
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return contracts.TokenBundle{}, executionError(ErrorInvalidEnvelope, false, err)
	}
	state, err := randomURLToken(32)
	if err != nil {
		return contracts.TokenBundle{}, executionError(ErrorInvalidEnvelope, false, err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return contracts.TokenBundle{}, executionError(ErrorCallbackListen, true, err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://localhost:%d/auth/callback", port)
	resultCh := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(state)) != 1 {
			http.Error(w, "State mismatch", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if errorCode := strings.TrimSpace(r.URL.Query().Get("error")); errorCode != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("Authorization was not completed."))
			deliverCallback(resultCh, callbackResult{err: executionError(ErrorCallbackDenied, false, errors.New(errorCode))})
			return
		}
		code := strings.TrimSpace(r.URL.Query().Get("code"))
		if code == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("Authorization code is missing."))
			deliverCallback(resultCh, callbackResult{err: executionError(ErrorCallbackInvalid, false, errors.New("authorization code is missing"))})
			return
		}
		_, _ = w.Write([]byte("Authorization received. Return to the command."))
		deliverCallback(resultCh, callbackResult{code: code})
	})
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()
	defer shutdownCallbackServer(server)

	authorizeURL, err := buildAuthorizeURL(profile, redirectURI, challenge, state)
	if err != nil {
		return contracts.TokenBundle{}, executionError(ErrorUnsupportedJob, false, err)
	}
	opener := e.OpenURL
	if opener == nil {
		opener = openBrowser
	}
	if err := opener(ctx, authorizeURL); err != nil {
		return contracts.TokenBundle{}, executionError(ErrorBrowserOpen, true, err)
	}
	timeout := e.CallbackTimeout
	if timeout <= 0 {
		timeout = defaultCallbackTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var callback callbackResult
	select {
	case <-ctx.Done():
		return contracts.TokenBundle{}, ctx.Err()
	case <-timer.C:
		return contracts.TokenBundle{}, executionError(ErrorCallbackTimeout, true, errors.New("OAuth callback timed out"))
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return contracts.TokenBundle{}, executionError(ErrorCallbackListen, true, errors.New("OAuth callback server stopped"))
		}
		return contracts.TokenBundle{}, executionError(ErrorCallbackListen, true, err)
	case callback = <-resultCh:
	}
	if callback.err != nil {
		return contracts.TokenBundle{}, callback.err
	}
	bundle, err := (providerapi.HTTPClient{Client: e.HTTPClient}).ExchangeCodeWithProxy(ctx, profile, callback.code, verifier, redirectURI, state, proxy)
	if err != nil {
		return contracts.TokenBundle{}, executionError(ErrorTokenExchange, true, err)
	}
	return bundle, nil
}

func defaultOAuthProfile(providerName string) (providerapi.EndpointProfile, bool) {
	if providerName != providerapi.KindCodex {
		return providerapi.EndpointProfile{}, false
	}
	return providerapi.CodexChatGPTProfile(), true
}

func buildAuthorizeURL(profile providerapi.EndpointProfile, redirectURI, challenge, state string) (string, error) {
	authorizeURL, err := url.Parse(profile.OAuthAuthorizeURL)
	if err != nil {
		return "", err
	}
	query := authorizeURL.Query()
	query.Set("response_type", "code")
	query.Set("client_id", profile.ClientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("scope", codexOAuthScope)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	query.Set("id_token_add_organizations", "true")
	query.Set("codex_cli_simplified_flow", "true")
	query.Set("state", state)
	query.Set("originator", "codex_cli_rs")
	authorizeURL.RawQuery = query.Encode()
	return authorizeURL.String(), nil
}

func generatePKCE() (string, string, error) {
	verifier, err := randomURLToken(64)
	if err != nil {
		return "", "", err
	}
	digest := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

func randomURLToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func newJobID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "oauth-" + hex.EncodeToString(value[:]), nil
}

func deliverCallback(ch chan<- callbackResult, result callbackResult) {
	select {
	case ch <- result:
	default:
	}
}

func shutdownCallbackServer(server *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

func openBrowser(ctx context.Context, target string) error {
	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		command, args = "open", []string{target}
	case "windows":
		command, args = "rundll32", []string{"url.dll,FileProtocolHandler", target}
	default:
		command, args = "xdg-open", []string{target}
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return err
	}
	commandProcess := exec.CommandContext(ctx, path, args...)
	if err := commandProcess.Start(); err != nil {
		return err
	}
	go func() { _ = commandProcess.Wait() }()
	return nil
}

func executionError(code string, retryable bool, err error) *ExecutionError {
	return &ExecutionError{Code: code, Retryable: retryable, Err: err}
}
