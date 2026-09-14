package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// B5.1: the read-only level exists to shrink the blast radius of the common
// case. These assert the split holds in both directions -- a reader cannot
// write, and a reader can still read -- because a level that blocks everything
// is not a level, it is a broken token.
func TestConsoleReadOnlyTokenMayReadButNotWrite(t *testing.T) {
	console := Console{Auth: ConsoleAuth{Token: "rw-secret", ReadOnlyToken: "ro-secret"}}
	handler := console.Handler()

	reader := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	reader.Header.Set("Authorization", "Bearer ro-secret")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, reader)
	if recorder.Code != http.StatusOK {
		t.Errorf("read-only token was refused a GET: status %d", recorder.Code)
	}

	// Every write path, not just one: the check is by method, and a test that
	// only covered one route would not notice a handler escaping the wrapper.
	for _, path := range []string{
		"/v1/billing/wallets/t1/topup",
		"/v1/billing/prices",
		"/v1/billing/held/e1/resolve",
		"/v1/accounts/a1/status",
		"/v1/accounts/a1/cooldown/clear",
		"/v1/accounts/a1/excluded-models/clear",
	} {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		request.Header.Set("Authorization", "Bearer ro-secret")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("POST %s with the read-only token = %d, want 403", path, recorder.Code)
		}
	}
}

func TestConsoleReadWriteTokenMayStillWrite(t *testing.T) {
	// DB is nil, so a permitted write reaches the handler and fails there with
	// 503. That is the point: anything other than 403 proves authorization let
	// it through, without needing a database to prove it.
	console := Console{Auth: ConsoleAuth{Token: "rw-secret", ReadOnlyToken: "ro-secret"}}
	request := httptest.NewRequest(http.MethodPost, "/v1/accounts/a1/status", strings.NewReader(`{"status":"disabled","reason":"test"}`))
	request.Header.Set("Authorization", "Bearer rw-secret")
	recorder := httptest.NewRecorder()
	console.Handler().ServeHTTP(recorder, request)
	if recorder.Code == http.StatusForbidden || recorder.Code == http.StatusUnauthorized {
		t.Fatalf("the read-write token was refused a write: status %d, body %s", recorder.Code, recorder.Body)
	}
}

// Without a read-only token configured, B5's behaviour must be unchanged: one
// token that may do everything. A hardening change that quietly broke every
// existing single-token deployment would not be hardening.
func TestConsoleWithoutAReadOnlyTokenKeepsTheSingleTokenBehaviour(t *testing.T) {
	console := Console{Auth: ConsoleAuth{Token: "only-secret"}}
	request := httptest.NewRequest(http.MethodPost, "/v1/accounts/a1/cooldown/clear", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer only-secret")
	recorder := httptest.NewRecorder()
	console.Handler().ServeHTTP(recorder, request)
	if recorder.Code == http.StatusForbidden {
		t.Fatalf("a single-token console refused its own write: %s", recorder.Body)
	}
}

// A read-only token alone must not enable the console. Booting with only the
// reader set is far more likely to be a mistake than a deliberate permanently
// read-only control plane.
func TestConsoleReadOnlyTokenAloneDoesNotEnableTheConsole(t *testing.T) {
	auth := ConsoleAuth{ReadOnlyToken: "ro-secret"}
	if auth.Enabled() {
		t.Fatal("a console with only a read-only token reported itself enabled")
	}
	console := Console{Auth: auth}
	request := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	request.Header.Set("Authorization", "Bearer ro-secret")
	recorder := httptest.NewRecorder()
	console.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401: a read-only token alone must not serve the console", recorder.Code)
	}
}

// Identical tokens have no safe reading, so Validate refuses rather than
// picking one. Run() calls this before listening.
func TestConsoleAuthRefusesIdenticalTokens(t *testing.T) {
	if err := (ConsoleAuth{Token: "same", ReadOnlyToken: "same"}).Validate(); err == nil {
		t.Fatal("identical read-write and read-only tokens were accepted")
	}
	// Whitespace must not be a way to sneak the same value past the check.
	if err := (ConsoleAuth{Token: "same", ReadOnlyToken: "  same  "}).Validate(); err == nil {
		t.Fatal("tokens differing only by surrounding whitespace were accepted")
	}
	if err := (ConsoleAuth{Token: "rw", ReadOnlyToken: "ro"}).Validate(); err != nil {
		t.Fatalf("distinct tokens were rejected: %v", err)
	}
	if err := (ConsoleAuth{Token: "rw"}).Validate(); err != nil {
		t.Fatalf("a console with no read-only level was rejected: %v", err)
	}
}

// The limiter must refuse past the burst and recover as the bucket refills.
func TestConsoleRateLimiterRefusesPastTheBurstAndRecovers(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	limiter := newConsoleLimiter(3, 1, func() time.Time { return now })
	for i := 0; i < 3; i++ {
		if allowed, _ := limiter.allow(consoleReadOnly); !allowed {
			t.Fatalf("request %d was refused inside the burst", i+1)
		}
	}
	allowed, wait := limiter.allow(consoleReadOnly)
	if allowed {
		t.Fatal("the burst did not cap")
	}
	if wait <= 0 {
		t.Errorf("Retry-After hint was %v, want a positive duration", wait)
	}
	now = now.Add(2 * time.Second)
	if allowed, _ := limiter.allow(consoleReadOnly); !allowed {
		t.Error("the bucket did not refill after two seconds at 1/s")
	}
}

// Reads and writes hold separate budgets. A dashboard polling the account board
// must not be able to starve the operator trying to resolve a held row -- that
// would turn a rate limit meant to protect the database into an outage of the
// console's whole point.
func TestConsoleRateLimiterKeepsReadsFromStarvingWrites(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	limiter := newConsoleLimiter(2, 1, func() time.Time { return now })
	for i := 0; i < 2; i++ {
		limiter.allow(consoleReadOnly)
	}
	if allowed, _ := limiter.allow(consoleReadOnly); allowed {
		t.Fatal("the read bucket did not cap")
	}
	if allowed, _ := limiter.allow(consoleReadWrite); !allowed {
		t.Fatal("an exhausted read bucket also refused a write")
	}
}

// A throttled request must say 429 with a Retry-After, not fail some other way.
func TestConsoleThrottlingReturns429WithRetryAfter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	console := Console{
		Auth:    ConsoleAuth{Token: "rw-secret"},
		Limiter: newConsoleLimiter(1, 1, func() time.Time { return now }),
	}
	handler := console.Handler()
	send := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		request.Header.Set("Authorization", "Bearer rw-secret")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	if got := send().Code; got != http.StatusOK {
		t.Fatalf("first request = %d, want 200", got)
	}
	second := send()
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429", second.Code)
	}
	if second.Header().Get("Retry-After") == "" {
		t.Error("a 429 carried no Retry-After, so a caller cannot know when to try again")
	}
}

// An unauthenticated flood must be rejected before it consumes tokens.
// Otherwise anyone who can reach the port can deny the console to the operator
// holding a real credential -- the limiter would become the attack.
func TestConsoleUnauthenticatedRequestsDoNotConsumeRateBudget(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	console := Console{
		Auth:    ConsoleAuth{Token: "rw-secret"},
		Limiter: newConsoleLimiter(1, 1, func() time.Time { return now }),
	}
	handler := console.Handler()
	for i := 0; i < 20; i++ {
		request := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		request.Header.Set("Authorization", "Bearer wrong")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated request %d = %d, want 401", i, recorder.Code)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	request.Header.Set("Authorization", "Bearer rw-secret")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("the real operator was throttled by someone else's failed attempts: %d", recorder.Code)
	}
}

// Cursors must survive a round trip, and a malformed one must be an error
// rather than a silent restart from the top -- a caller quietly sent back to
// row one loops forever over the same page.
func TestConsoleCursorRoundTripsAndRejectsGarbage(t *testing.T) {
	encoded := encodeCursor("2026-09-14T00:00:00Z", 42)
	decoded, err := parseCursor(encoded)
	if err != nil {
		t.Fatalf("parse of an encoded cursor failed: %v", err)
	}
	if decoded.Sort != "2026-09-14T00:00:00Z" || decoded.ID != 42 {
		t.Errorf("round trip lost data: %+v", decoded)
	}
	if empty, err := parseCursor(""); err != nil || empty.ID != 0 || empty.Sort != "" {
		t.Errorf("an empty cursor must mean the beginning, got %+v / %v", empty, err)
	}
	for _, bad := range []string{"not-base64!!", "YWJj", encodeCursor("x", 1) + "@@"} {
		if _, err := parseCursor(bad); err == nil {
			t.Errorf("cursor %q was accepted; a bad cursor must not silently restart paging", bad)
		}
	}
}
