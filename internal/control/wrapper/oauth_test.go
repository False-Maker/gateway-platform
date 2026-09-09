package wrapper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

func TestAuthorizeClientRunsCodexPKCEThroughWorker(t *testing.T) {
	cipher := testEnvelopeCipher(t)
	var (
		fixture       *httptest.Server
		fixtureMu     sync.Mutex
		fixtureErr    error
		challenge     string
		authorizeHits int
		tokenHits     int
		mismatchSeen  bool
	)
	recordFixtureError := func(err error) {
		fixtureMu.Lock()
		defer fixtureMu.Unlock()
		if fixtureErr == nil {
			fixtureErr = err
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		for name, want := range map[string]string{
			"response_type":              "code",
			"client_id":                  "fixture-client",
			"scope":                      codexOAuthScope,
			"code_challenge_method":      "S256",
			"id_token_add_organizations": "true",
			"codex_cli_simplified_flow":  "true",
			"originator":                 "codex_cli_rs",
		} {
			if got := query.Get(name); got != want {
				recordFixtureError(fmt.Errorf("authorize %s=%q want %q", name, got, want))
				http.Error(w, "invalid authorize request", http.StatusBadRequest)
				return
			}
		}
		if !strings.HasPrefix(query.Get("redirect_uri"), "http://localhost:") || !strings.HasSuffix(query.Get("redirect_uri"), "/auth/callback") || query.Get("state") == "" || query.Get("code_challenge") == "" {
			recordFixtureError(errors.New("authorize request is missing PKCE callback fields"))
			http.Error(w, "invalid authorize request", http.StatusBadRequest)
			return
		}
		fixtureMu.Lock()
		authorizeHits++
		challenge = query.Get("code_challenge")
		fixtureMu.Unlock()
		callback, err := url.Parse(query.Get("redirect_uri"))
		if err != nil {
			recordFixtureError(err)
			http.Error(w, "invalid redirect", http.StatusBadRequest)
			return
		}
		callbackQuery := callback.Query()
		callbackQuery.Set("code", "fixture-code")
		callbackQuery.Set("state", query.Get("state"))
		callback.RawQuery = callbackQuery.Encode()
		http.Redirect(w, r, callback.String(), http.StatusFound)
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			recordFixtureError(err)
			http.Error(w, "invalid token request", http.StatusBadRequest)
			return
		}
		fixtureMu.Lock()
		wantChallenge := challenge
		tokenHits++
		fixtureMu.Unlock()
		digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		gotChallenge := base64.RawURLEncoding.EncodeToString(digest[:])
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "fixture-code" || r.Form.Get("client_id") != "fixture-client" || !strings.HasPrefix(r.Form.Get("redirect_uri"), "http://localhost:") || gotChallenge != wantChallenge {
			recordFixtureError(errors.New("token request did not preserve authorization or PKCE values"))
			http.Error(w, "invalid token request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fixture-access","refresh_token":"fixture-refresh","id_token":"fixture-id","expires_in":3600,"scope":"openid profile"}`))
	})
	fixture = httptest.NewServer(mux)
	defer fixture.Close()
	profile := fixtureCodexProfile(fixture.URL)

	opener := func(ctx context.Context, authorizeURL string) error {
		parsed, err := url.Parse(authorizeURL)
		if err != nil {
			return err
		}
		callback, err := url.Parse(parsed.Query().Get("redirect_uri"))
		if err != nil {
			return err
		}
		wrongState := callback.Query()
		wrongState.Set("code", "wrong-state-code")
		wrongState.Set("state", "wrong-state")
		callback.RawQuery = wrongState.Encode()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, callback.String(), nil)
		if err != nil {
			return err
		}
		response, err := fixture.Client().Do(request)
		if err != nil {
			return err
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			return fmt.Errorf("state mismatch status=%d", response.StatusCode)
		}
		fixtureMu.Lock()
		mismatchSeen = true
		fixtureMu.Unlock()

		request, err = http.NewRequestWithContext(ctx, http.MethodGet, authorizeURL, nil)
		if err != nil {
			return err
		}
		response, err = fixture.Client().Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("authorize status=%d", response.StatusCode)
		}
		return nil
	}

	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	queue := Queue{Redis: rdb}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (Worker{
			Queue:           queue,
			WorkerID:        "pkce-worker",
			LeaseTTLSeconds: 30,
			PollInterval:    time.Millisecond,
			Executor: PKCEExecutor{
				Cipher:          cipher,
				HTTPClient:      fixture.Client(),
				OpenURL:         opener,
				CallbackTimeout: time.Second,
				Profile: func(string) (providerapi.EndpointProfile, bool) {
					return profile, true
				},
			},
		}).Run(ctx)
	}()
	authorizeCtx, authorizeCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer authorizeCancel()
	bundle, err := (AuthorizeClient{Queue: queue, Cipher: cipher, PollInterval: time.Millisecond}).Authorize(authorizeCtx, providerapi.KindCodex)
	if err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("worker cancellation: %v", err)
	}
	if bundle.AccessToken != "fixture-access" || bundle.RefreshToken != "fixture-refresh" || bundle.IDToken != "fixture-id" || len(bundle.Scopes) != 2 || bundle.ExpiresAt.IsZero() {
		t.Fatalf("unexpected token bundle: %#v", bundle)
	}
	fixtureMu.Lock()
	defer fixtureMu.Unlock()
	if fixtureErr != nil {
		t.Fatal(fixtureErr)
	}
	if authorizeHits != 1 || tokenHits != 1 || !mismatchSeen {
		t.Fatalf("fixture authorize_hits=%d token_hits=%d mismatch_seen=%t", authorizeHits, tokenHits, mismatchSeen)
	}
}

func TestPKCEExecutorClassifiesDeniedCallback(t *testing.T) {
	cipher := testEnvelopeCipher(t)
	mux := http.NewServeMux()
	fixture := httptest.NewServer(mux)
	defer fixture.Close()
	mux.HandleFunc("/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		callback, err := url.Parse(r.URL.Query().Get("redirect_uri"))
		if err != nil {
			http.Error(w, "invalid redirect", http.StatusBadRequest)
			return
		}
		query := callback.Query()
		query.Set("error", "access_denied")
		query.Set("state", r.URL.Query().Get("state"))
		callback.RawQuery = query.Encode()
		http.Redirect(w, r, callback.String(), http.StatusFound)
	})
	job := testAuthorizeJob(t, cipher, providerapi.KindCodex)
	_, err := (PKCEExecutor{
		Cipher:     cipher,
		HTTPClient: fixture.Client(),
		OpenURL: func(ctx context.Context, target string) error {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err != nil {
				return err
			}
			response, err := fixture.Client().Do(request)
			if err != nil {
				return err
			}
			_ = response.Body.Close()
			return nil
		},
		CallbackTimeout: time.Second,
		Profile: func(string) (providerapi.EndpointProfile, bool) {
			return fixtureCodexProfile(fixture.URL), true
		},
	}).Execute(context.Background(), job)
	var executionErr *ExecutionError
	if !errors.As(err, &executionErr) || executionErr.Code != ErrorCallbackDenied || executionErr.Retryable {
		t.Fatalf("unexpected denied callback error: %#v", err)
	}
}

func TestPKCEExecutorRejectsUnsupportedDispatch(t *testing.T) {
	cipher := testEnvelopeCipher(t)
	job := testAuthorizeJob(t, cipher, providerapi.KindClaude)
	_, err := (PKCEExecutor{Cipher: cipher}).Execute(context.Background(), job)
	var executionErr *ExecutionError
	if !errors.As(err, &executionErr) || executionErr.Code != ErrorUnsupportedJob || executionErr.Retryable {
		t.Fatalf("unexpected unsupported dispatch error: %#v", err)
	}
}

func testEnvelopeCipher(t *testing.T) *EnvelopeCipher {
	t.Helper()
	encoded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x6a}, 32))
	cipher, err := NewEnvelopeCipher(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}

func testAuthorizeJob(t *testing.T, cipher *EnvelopeCipher, providerName string) contracts.WrapperJob {
	t.Helper()
	job := contracts.WrapperJob{
		SchemaVersion: contracts.SchemaVersion,
		JobID:         "authorize-test-job",
		Provider:      providerName,
		Operation:     contracts.WrapperAuthorize,
	}
	plaintext, err := json.Marshal(authorizeInput{AuthMode: "pkce"})
	if err != nil {
		t.Fatal(err)
	}
	job.EncryptedInput, err = cipher.Encrypt(job.JobID, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func fixtureCodexProfile(baseURL string) providerapi.EndpointProfile {
	return providerapi.EndpointProfile{
		Kind:              providerapi.KindCodex,
		AuthMode:          providerapi.AuthModeOAuth,
		OAuthAuthorizeURL: baseURL + "/oauth/authorize",
		OAuthTokenURL:     baseURL + "/oauth/token",
		ClientID:          "fixture-client",
		APIBaseURL:        baseURL,
		InferencePath:     "/responses",
	}
}
