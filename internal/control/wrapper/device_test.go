package wrapper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

// deviceFixture is a local RFC 8628 authorization server. It exists so the
// polling loop, the parameter generation and the error classification can be
// exercised without any real account: the real code exchange belongs to C4.
type deviceFixture struct {
	server *httptest.Server

	mu           sync.Mutex
	deviceHits   int
	tokenHits    int
	pollTimes    []time.Time
	deviceParams map[string]string
	tokenParams  map[string]string
	err          error

	// responses is consumed one entry per token poll; the last entry repeats.
	responses []string
	interval  int64
	expiresIn int64
}

func newDeviceFixture(t *testing.T, responses []string) *deviceFixture {
	t.Helper()
	fixture := &deviceFixture{responses: responses, expiresIn: 900}
	mux := http.NewServeMux()
	mux.HandleFunc("/device/code", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			fixture.record(err)
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		fixture.mu.Lock()
		fixture.deviceHits++
		fixture.deviceParams = flatten(r.PostForm)
		interval, expires := fixture.interval, fixture.expiresIn
		fixture.mu.Unlock()
		if r.Header.Get("Accept") != "application/json" {
			fixture.record(errors.New("device code request did not ask for JSON"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"device_code":"fixture-device-code","user_code":"WDJB-MJHT",`+
			`"verification_uri":"https://example.test/activate",`+
			`"verification_uri_complete":"https://example.test/activate?user_code=WDJB-MJHT",`+
			`"expires_in":%d,"interval":%d}`, expires, interval)
	})
	mux.HandleFunc("/device/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			fixture.record(err)
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		fixture.mu.Lock()
		fixture.tokenHits++
		fixture.pollTimes = append(fixture.pollTimes, time.Now())
		fixture.tokenParams = flatten(r.PostForm)
		index := fixture.tokenHits - 1
		if index >= len(fixture.responses) {
			index = len(fixture.responses) - 1
		}
		body := fixture.responses[index]
		fixture.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		// RFC 8628 §3.5 carries authorization_pending and slow_down as OAuth
		// error responses, which are 400s. The fixture answers the way a real
		// server does so the executor is forced to read the error field rather
		// than the status code.
		if strings.Contains(body, `"error"`) {
			w.WriteHeader(http.StatusBadRequest)
		}
		_, _ = w.Write([]byte(body))
	})
	fixture.server = httptest.NewServer(mux)
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (f *deviceFixture) record(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err == nil {
		f.err = err
	}
}

func (f *deviceFixture) profile() DeviceProfile {
	return DeviceProfile{
		DeviceCodeURL: f.server.URL + "/device/code",
		TokenURL:      f.server.URL + "/device/token",
		ClientID:      "fixture-device-client",
		Scope:         "read:user",
	}
}

func (f *deviceFixture) snapshot() (deviceHits, tokenHits int, deviceParams, tokenParams map[string]string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deviceHits, f.tokenHits, f.deviceParams, f.tokenParams, f.err
}

func flatten(values map[string][]string) map[string]string {
	flat := make(map[string]string, len(values))
	for key, entries := range values {
		if len(entries) > 0 {
			flat[key] = entries[0]
		}
	}
	return flat
}

func deviceJob(t *testing.T, cipher *EnvelopeCipher, providerName, authMode string) contracts.WrapperJob {
	t.Helper()
	job := contracts.WrapperJob{
		SchemaVersion: contracts.SchemaVersion,
		JobID:         "device-test-job",
		Provider:      providerName,
		Operation:     contracts.WrapperAuthorize,
	}
	plaintext, err := json.Marshal(authorizeInput{AuthMode: authMode})
	if err != nil {
		t.Fatal(err)
	}
	job.EncryptedInput, err = cipher.Encrypt(job.JobID, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func decodeAuthorizeOutput(t *testing.T, cipher *EnvelopeCipher, jobID string, encrypted []byte) authorizeOutput {
	t.Helper()
	plaintext, err := cipher.Decrypt(jobID, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	var output authorizeOutput
	if err := json.Unmarshal(plaintext, &output); err != nil {
		t.Fatal(err)
	}
	return output
}

func TestDeviceExecutorRunsTheFullFlowAgainstAFixtureServer(t *testing.T) {
	cipher := testEnvelopeCipher(t)
	fixture := newDeviceFixture(t, []string{
		`{"error":"authorization_pending"}`,
		`{"error":"authorization_pending"}`,
		`{"access_token":"device-access","refresh_token":"device-refresh","expires_in":3600,"scope":"read:user repo"}`,
	})
	var prompted DeviceInstruction
	executor := DeviceExecutor{
		Cipher:       cipher,
		Profile:      func(string) (DeviceProfile, bool) { return fixture.profile(), true },
		PollInterval: time.Millisecond,
		Prompt: func(_ context.Context, instruction DeviceInstruction) error {
			prompted = instruction
			return nil
		},
	}

	job := deviceJob(t, cipher, providerapi.KindCopilot, deviceAuthMode)
	encrypted, err := executor.Execute(context.Background(), job)
	if err != nil {
		t.Fatalf("device authorization failed: %v", err)
	}

	deviceHits, tokenHits, deviceParams, tokenParams, fixtureErr := fixture.snapshot()
	if fixtureErr != nil {
		t.Fatal(fixtureErr)
	}
	if deviceHits != 1 {
		t.Fatalf("device code endpoint hit %d times, want 1", deviceHits)
	}
	if tokenHits != 3 {
		t.Fatalf("token endpoint hit %d times, want 3 (two pending then the grant)", tokenHits)
	}
	if deviceParams["client_id"] != "fixture-device-client" || deviceParams["scope"] != "read:user" {
		t.Fatalf("device code request carried the wrong parameters: %v", deviceParams)
	}
	if tokenParams["grant_type"] != deviceGrantType {
		t.Fatalf("token request grant_type=%q, want %q", tokenParams["grant_type"], deviceGrantType)
	}
	if tokenParams["device_code"] != "fixture-device-code" || tokenParams["client_id"] != "fixture-device-client" {
		t.Fatalf("token request carried the wrong parameters: %v", tokenParams)
	}

	// The user code is the only part of this flow a human must act on; losing
	// it means the job can only ever time out.
	if prompted.UserCode != "WDJB-MJHT" || prompted.VerificationURI != "https://example.test/activate" {
		t.Fatalf("prompt did not receive usable instructions: %#v", prompted)
	}
	if prompted.VerificationURIComplete == "" || prompted.ExpiresIn != 900*time.Second {
		t.Fatalf("prompt is missing the completed URI or the expiry: %#v", prompted)
	}

	bundle := decodeAuthorizeOutput(t, cipher, job.JobID, encrypted).TokenBundle
	if bundle.AccessToken != "device-access" || bundle.RefreshToken != "device-refresh" {
		t.Fatalf("unexpected token bundle: %#v", bundle)
	}
	if len(bundle.Scopes) != 2 || bundle.Scopes[0] != "read:user" || bundle.Scopes[1] != "repo" {
		t.Fatalf("scopes were not split from the scope string: %#v", bundle.Scopes)
	}
	if bundle.ExpiresAt.IsZero() {
		t.Fatal("expires_in was not turned into an absolute expiry")
	}
}

func TestDeviceExecutorSlowsDownWhenAsked(t *testing.T) {
	cipher := testEnvelopeCipher(t)
	fixture := newDeviceFixture(t, []string{
		`{"error":"slow_down"}`,
		`{"access_token":"device-access"}`,
	})
	const increment = 80 * time.Millisecond
	executor := DeviceExecutor{
		Cipher:            cipher,
		Profile:           func(string) (DeviceProfile, bool) { return fixture.profile(), true },
		PollInterval:      time.Millisecond,
		SlowDownIncrement: increment,
		Prompt:            func(context.Context, DeviceInstruction) error { return nil },
	}

	if _, err := executor.Execute(context.Background(), deviceJob(t, cipher, providerapi.KindCopilot, deviceAuthMode)); err != nil {
		t.Fatalf("device authorization failed: %v", err)
	}

	fixture.mu.Lock()
	times := append([]time.Time(nil), fixture.pollTimes...)
	fixture.mu.Unlock()
	if len(times) != 2 {
		t.Fatalf("token endpoint hit %d times, want 2", len(times))
	}
	// Without honouring slow_down the second poll would follow the 1ms poll
	// interval, so this gap is the whole assertion.
	if gap := times[1].Sub(times[0]); gap < increment {
		t.Fatalf("slow_down was ignored: second poll came after %s, want at least %s", gap, increment)
	}
}

func TestDeviceExecutorClassifiesPollingErrors(t *testing.T) {
	cases := []struct {
		name      string
		response  string
		code      string
		retryable bool
	}{
		// Denied is the user's answer; asking again gets the same answer.
		{"denied", `{"error":"access_denied"}`, ErrorDeviceDenied, false},
		// Expired is about this code, not this account: a new job gets a new code.
		{"expired", `{"error":"expired_token"}`, ErrorDeviceExpired, true},
		{"unknown", `{"error":"invalid_client"}`, ErrorDeviceTokenInvalid, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cipher := testEnvelopeCipher(t)
			fixture := newDeviceFixture(t, []string{testCase.response})
			executor := DeviceExecutor{
				Cipher:       cipher,
				Profile:      func(string) (DeviceProfile, bool) { return fixture.profile(), true },
				PollInterval: time.Millisecond,
				Prompt:       func(context.Context, DeviceInstruction) error { return nil },
			}
			_, err := executor.Execute(context.Background(), deviceJob(t, cipher, providerapi.KindCopilot, deviceAuthMode))
			assertExecutionError(t, err, testCase.code, testCase.retryable)
		})
	}
}

func TestDeviceExecutorTimesOutWhileStillPending(t *testing.T) {
	cipher := testEnvelopeCipher(t)
	fixture := newDeviceFixture(t, []string{`{"error":"authorization_pending"}`})
	executor := DeviceExecutor{
		Cipher:       cipher,
		Profile:      func(string) (DeviceProfile, bool) { return fixture.profile(), true },
		PollInterval: time.Millisecond,
		Timeout:      60 * time.Millisecond,
		Prompt:       func(context.Context, DeviceInstruction) error { return nil },
	}
	_, err := executor.Execute(context.Background(), deviceJob(t, cipher, providerapi.KindCopilot, deviceAuthMode))
	assertExecutionError(t, err, ErrorDeviceTimeout, true)
}

func TestDeviceExecutorRejectsAnIncompleteDeviceCodeResponse(t *testing.T) {
	cipher := testEnvelopeCipher(t)
	fixture := newDeviceFixture(t, []string{`{"access_token":"device-access"}`})
	executor := DeviceExecutor{
		Cipher: cipher,
		Profile: func(string) (DeviceProfile, bool) {
			profile := fixture.profile()
			// Point the device-code request at the token endpoint, which
			// answers without a user_code.
			profile.DeviceCodeURL = profile.TokenURL
			return profile, true
		},
		PollInterval: time.Millisecond,
		Prompt:       func(context.Context, DeviceInstruction) error { return nil },
	}
	_, err := executor.Execute(context.Background(), deviceJob(t, cipher, providerapi.KindCopilot, deviceAuthMode))
	assertExecutionError(t, err, ErrorDeviceCodeRequest, false)
}

// The device access-token endpoint is deliberately not wired in (see
// defaultDeviceProfile: polling it alone yields a GitHub token, not a Copilot
// credential). The behaviour that matters is that a production worker refuses
// the job by name rather than completing it with an unusable token.
func TestDefaultDeviceProfileIsIncompleteAndFailsExplicitly(t *testing.T) {
	profile, ok := defaultDeviceProfile(providerapi.KindCopilot)
	if !ok {
		t.Fatal("copilot does not resolve to a device profile at all")
	}
	if profile.DeviceCodeURL != providerapi.GitHubCopilotProfile().OAuthAuthorizeURL || profile.ClientID == "" {
		t.Fatalf("device profile lost the parts that are established in-repo: %#v", profile)
	}
	if profile.TokenURL != "" {
		t.Fatal("device profile now claims a token endpoint; that is only correct once the " +
			"GitHub-token -> Copilot-token exchange also exists, otherwise the job completes with an unusable token")
	}

	cipher := testEnvelopeCipher(t)
	executor := DeviceExecutor{Cipher: cipher, Prompt: func(context.Context, DeviceInstruction) error { return nil }}
	_, err := executor.Execute(context.Background(), deviceJob(t, cipher, providerapi.KindCopilot, deviceAuthMode))
	assertExecutionError(t, err, ErrorDeviceProfilePending, false)

	if _, ok := defaultDeviceProfile(providerapi.KindCodex); ok {
		t.Fatal("codex resolved to a device profile; codex uses PKCE")
	}
}

func TestDeviceExecutorRejectsANonDeviceEnvelope(t *testing.T) {
	cipher := testEnvelopeCipher(t)
	executor := DeviceExecutor{Cipher: cipher}
	_, err := executor.Execute(context.Background(), deviceJob(t, cipher, providerapi.KindCopilot, "pkce"))
	assertExecutionError(t, err, ErrorInvalidEnvelope, false)
}

func TestAuthModeRouterDispatchesOnTheEnvelope(t *testing.T) {
	cipher := testEnvelopeCipher(t)
	var routed string
	router := authModeRouter{
		Cipher: cipher,
		Executors: map[string]Executor{
			"pkce":         executorFunc(func() { routed = "pkce" }),
			deviceAuthMode: executorFunc(func() { routed = "device" }),
		},
	}

	if _, err := router.Execute(context.Background(), deviceJob(t, cipher, providerapi.KindCopilot, deviceAuthMode)); err != nil {
		t.Fatal(err)
	}
	if routed != "device" {
		t.Fatalf("device job routed to %q", routed)
	}
	if _, err := router.Execute(context.Background(), deviceJob(t, cipher, providerapi.KindCodex, "pkce")); err != nil {
		t.Fatal(err)
	}
	if routed != "pkce" {
		t.Fatalf("pkce job routed to %q", routed)
	}

	_, err := router.Execute(context.Background(), deviceJob(t, cipher, providerapi.KindCopilot, "cli"))
	assertExecutionError(t, err, ErrorUnsupportedJob, false)
}

// A device failure only becomes actionable on the control side if the worker
// turns it into a WrapperFailure code rather than the generic executor_failed.
func TestDeviceErrorsSurviveWorkerClassification(t *testing.T) {
	code, retryable := classifyExecutionError(executionError(ErrorDeviceDenied, false, errors.New("denied")))
	if code != ErrorDeviceDenied || retryable {
		t.Fatalf("worker classified a denial as code=%q retryable=%t", code, retryable)
	}
	code, retryable = classifyExecutionError(executionError(ErrorDeviceProfilePending, false, errors.New("pending")))
	if code != ErrorDeviceProfilePending || retryable {
		t.Fatalf("worker classified a pending profile as code=%q retryable=%t", code, retryable)
	}
}

type executorFunc func()

func (f executorFunc) Execute(context.Context, contracts.WrapperJob) ([]byte, error) {
	f()
	return []byte{1}, nil
}

func assertExecutionError(t *testing.T, err error, code string, retryable bool) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got no error", code)
	}
	var executionErr *ExecutionError
	if !errors.As(err, &executionErr) {
		t.Fatalf("expected an *ExecutionError, got %T: %v", err, err)
	}
	if executionErr.Code != code {
		t.Fatalf("error code=%q want %q (%v)", executionErr.Code, code, err)
	}
	if executionErr.Retryable != retryable {
		t.Fatalf("error %s retryable=%t want %t", code, executionErr.Retryable, retryable)
	}
}
