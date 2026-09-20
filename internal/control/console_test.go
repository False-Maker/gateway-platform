package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elucid/gateway-platform/internal/observability"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

// newTestConsole builds a console with no database and no detail store, which
// is the deployment where the console must still answer and must say why a
// route cannot. Tests that need PostgreSQL live in console_integration_test.go.
func newTestConsole(token string) Console {
	return Console{Auth: ConsoleAuth{Token: token}, Now: func() time.Time {
		return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	}}
}

func consoleRequest(t *testing.T, handler http.Handler, method, target, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// The console must not exist without a token. A missing environment variable
// becoming an unauthenticated control API would be the worst failure mode this
// file could have, so it is asserted rather than assumed.
func TestConsoleRefusesEverythingWhenDisabled(t *testing.T) {
	handler := newTestConsole("").Handler()
	for _, target := range []string{"/v1/health", "/v1/accounts", "/v1/billing/held", "/v1/requests"} {
		response := consoleRequest(t, handler, http.MethodGet, target, "", "")
		if response.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a configured token: got %d, want 401", target, response.Code)
		}
	}
}

func TestConsoleRejectsWrongOrMalformedTokens(t *testing.T) {
	handler := newTestConsole("operator-secret").Handler()
	for name, authorization := range map[string]string{
		"absent":         "",
		"raw token":      "operator-secret",
		"wrong bearer":   "Bearer not-the-token",
		"empty bearer":   "Bearer ",
		"empty bearer 2": "Bearer",
		"prefix only":    "Basic operator-secret",
		"trailing junk":  "Bearer operator-secret-extra",
	} {
		request := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", name, recorder.Code)
		}
	}
	// The scheme is matched case-insensitively, as HTTP requires.
	request := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	request.Header.Set("Authorization", "bearer operator-secret")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Errorf("lowercase bearer scheme: got %d, want 200", recorder.Code)
	}
}

// Every route is behind the same check, including ones that would be harmless
// on their own. A per-handler check is one edit away from being forgotten on
// the next route; wrapping the mux is not.
func TestConsoleAuthorizesEveryRoute(t *testing.T) {
	handler := newTestConsole("operator-secret").Handler()
	routes := []struct{ method, target, body string }{
		{http.MethodGet, "/v1/health", ""},
		{http.MethodGet, "/v1/accounts", ""},
		{http.MethodGet, "/v1/billing/wallets/t1", ""},
		{http.MethodPost, "/v1/billing/wallets/t1/topup", `{"id":"x","amount":"1"}`},
		{http.MethodPost, "/v1/billing/wallets/t1/adjust", `{"id":"x","amount":"-1","reason":"r"}`},
		{http.MethodGet, "/v1/billing/prices", ""},
		{http.MethodPost, "/v1/billing/prices", `{"id":"p"}`},
		{http.MethodGet, "/v1/billing/held", ""},
		{http.MethodPost, "/v1/billing/held/e1/resolve", `{"decision":"write_off"}`},
		{http.MethodGet, "/v1/billing/reconcile", ""},
		{http.MethodGet, "/v1/requests", ""},
	}
	for _, route := range routes {
		response := consoleRequest(t, handler, route.method, route.target, "", route.body)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("%s %s unauthenticated: got %d, want 401", route.method, route.target, response.Code)
		}
	}
}

// With the token but no PostgreSQL, the database-backed reads must say so
// rather than panic or report an empty result: "no database" and "nothing
// matched" are different answers.
func TestConsoleReportsMissingDependencies(t *testing.T) {
	handler := newTestConsole("operator-secret").Handler()
	for _, target := range []string{"/v1/accounts", "/v1/billing/held", "/v1/billing/prices"} {
		response := consoleRequest(t, handler, http.MethodGet, target, "operator-secret", "")
		if response.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s without a database: got %d, want 503", target, response.Code)
		}
	}
	response := consoleRequest(t, handler, http.MethodGet, "/v1/requests", "operator-secret", "")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /v1/requests without a detail store: got %d, want 503", response.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// The reason has to name the actual cause, or an operator will look for
	// traffic that was never the problem.
	if !strings.Contains(body["error"], "detail store is not configured") {
		t.Errorf("detail-disabled error %q does not explain that the store is absent", body["error"])
	}
}

func TestConsoleRejectsMalformedBodies(t *testing.T) {
	handler := newTestConsole("operator-secret").Handler()
	cases := map[string]struct {
		target string
		body   string
		status int
	}{
		"unknown field": {
			target: "/v1/billing/wallets/t1/topup",
			body:   `{"id":"x","amount":"1","amountt":"10"}`,
			status: http.StatusBadRequest,
		},
		"single document": {
			target: "/v1/billing/wallets/t1/topup",
			body:   `{"id":"x","amount":"1"}{"id":"y","amount":"2"}`,
			status: http.StatusBadRequest,
		},
		"bad decision": {
			target: "/v1/billing/held/e1/resolve",
			body:   `{"decision":"maybe"}`,
			status: http.StatusBadRequest,
		},
		"missing decision": {
			target: "/v1/billing/held/e1/resolve",
			body:   `{}`,
			status: http.StatusBadRequest,
		},
	}
	for name, testCase := range cases {
		response := consoleRequest(t, handler, http.MethodPost, testCase.target, "operator-secret", testCase.body)
		if response.Code != testCase.status {
			t.Errorf("%s: got %d, want %d (%s)", name, response.Code, testCase.status, response.Body.String())
		}
	}
}

// A top-up without an idempotency key is refused before any money moves, and so
// is a non-positive amount. Both are checked in the handler precisely so that
// no database call is reached.
//
// A bare JSON number is deliberately absent from this list: contracts.Decimal
// accepts both "10" and 10 on the wire, so rejecting the unquoted form here
// would be inventing a stricter rule than the rest of the platform's API.
func TestConsoleTopupValidatesBeforeTouchingTheDatabase(t *testing.T) {
	handler := newTestConsole("operator-secret").Handler()
	for name, body := range map[string]string{
		"no id":           `{"amount":"10"}`,
		"zero amount":     `{"id":"x","amount":"0"}`,
		"negative amount": `{"id":"x","amount":"-10"}`,
		"non-decimal":     `{"id":"x","amount":"ten"}`,
		"empty body":      ``,
	} {
		response := consoleRequest(t, handler, http.MethodPost, "/v1/billing/wallets/t1/topup", "operator-secret", body)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400 (%s)", name, response.Code, response.Body.String())
		}
	}
}

// Reconcile requires an explicit window. B4.5 chose occurred_at as the window
// column precisely because it is a choice, so the console must not pick a
// default one on the operator's behalf.
func TestConsoleReconcileRequiresAnExplicitWindow(t *testing.T) {
	handler := newTestConsole("operator-secret").Handler()
	for name, target := range map[string]string{
		"no window":  "/v1/billing/reconcile",
		"start only": "/v1/billing/reconcile?start=2026-09-01T00:00:00Z",
		"end only":   "/v1/billing/reconcile?end=2026-09-01T00:00:00Z",
		"bad start":  "/v1/billing/reconcile?start=yesterday&end=2026-09-01T00:00:00Z",
	} {
		response := consoleRequest(t, handler, http.MethodGet, target, "operator-secret", "")
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", name, response.Code)
		}
	}
}

func TestConsoleRejectsBadRequestFilters(t *testing.T) {
	handler := newTestConsole("operator-secret").Handler()
	for name, target := range map[string]string{
		"bad limit":   "/v1/requests?limit=0",
		"bad limit 2": "/v1/requests?limit=many",
		"bad window":  "/v1/requests?start=2026-09-01",
	} {
		response := consoleRequest(t, handler, http.MethodGet, target, "operator-secret", "")
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", name, response.Code)
		}
	}
}

// The operator header is attribution, not authentication: it is echoed into the
// audit row and it must never be able to change whether a request is allowed.
func TestOperatorHeaderIsNotAuthorization(t *testing.T) {
	handler := newTestConsole("operator-secret").Handler()
	request := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	request.Header.Set("X-Operator", "operator-secret")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("an operator header alone authorized a request: got %d", recorder.Code)
	}
	if who := operator(request); who != "operator-secret" {
		t.Errorf("operator claim is recorded verbatim as %q", who)
	}
	// An absent claim is named, not empty: an audit row nobody can attribute is
	// less useful than one that says the caller did not say who they were.
	if who := operator(httptest.NewRequest(http.MethodGet, "/v1/health", nil)); who != "unknown" {
		t.Errorf("absent operator claim: got %q, want \"unknown\"", who)
	}
}

// A price must take effect in the future. D3 makes that a property of the
// table, but the console has to reject it before the database does so the
// operator gets an explanation rather than a constraint violation.
func TestConsolePriceRejectsRetroactiveEffect(t *testing.T) {
	handler := newTestConsole("operator-secret").Handler()
	response := consoleRequest(t, handler, http.MethodPost, "/v1/billing/prices", "operator-secret",
		`{"id":"p1","provider":"anthropic","model":"claude-opus-5","unit_scale":1000000,`+
			`"price_input":"3","price_output":"15","price_cache_read":"0.3","price_cache_write":"3.75",`+
			`"effective_from":"2026-09-01T00:00:00Z"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("retroactive price: got %d, want 400 (%s)", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "D3") {
		t.Errorf("rejection does not name the rule it enforced: %s", response.Body.String())
	}
	response = consoleRequest(t, handler, http.MethodPost, "/v1/billing/prices", "operator-secret",
		`{"id":"p1","provider":"anthropic","model":"claude-opus-5","unit_scale":1000000,`+
			`"price_input":"3","price_output":"15","price_cache_read":"0.3","price_cache_write":"3.75"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing effective_from: got %d, want 400", response.Code)
	}
	if !strings.Contains(response.Body.String(), "effective_from") {
		t.Errorf("missing effective_from is not named: %s", response.Body.String())
	}
}

// heldReason restates the policy table rather than inventing its own wording,
// so a row can never be described differently from the reason it was held.
func TestHeldReasonMatchesThePolicyTable(t *testing.T) {
	for name, testCase := range map[string]struct {
		source     string
		partial    bool
		statusCode int
		errorClass string
		wantSubstr string
	}{
		"served with missing usage": {
			source: "missing", statusCode: 200, errorClass: "ok",
			wantSubstr: "never reported token counts",
		},
		"estimated usage": {
			source: "estimated", statusCode: 200, errorClass: "ok",
			wantSubstr: "estimated, not measured",
		},
		"unknown class": {
			source: "something-new", statusCode: 200, errorClass: "ok",
			wantSubstr: "policy table is behind the contract",
		},
	} {
		reason := heldReason(testCase.source, testCase.partial, testCase.statusCode, testCase.errorClass)
		if !strings.Contains(reason, testCase.wantSubstr) {
			t.Errorf("%s: reason %q does not contain %q", name, reason, testCase.wantSubstr)
		}
	}
	// Every class the policy table holds must get a reason, otherwise the tray
	// would show a held row with no explanation.
	for class, disposition := range usageDispositions {
		if disposition != dispositionHold {
			continue
		}
		reason := heldReason(class.source, class.partial, 200, string(errorClassFor(class.succeeded)))
		if strings.TrimSpace(reason) == "" {
			t.Errorf("held class %+v has no explanation", class)
		}
	}
}

// walletBlocked has to agree with the rule B4.4's publish tick applies,
// otherwise the console would tell an operator a tenant is fine while the
// gateway refuses it with 402.
func TestWalletBlockedMatchesThePublishRule(t *testing.T) {
	for amount, want := range map[string]bool{
		"0":               true,
		"-0.000000000001": true,
		"-100":            true,
		"0.000000000001":  false,
		"100":             false,
	} {
		if got := walletBlocked(contracts.Decimal(amount)); got != want {
			t.Errorf("balance %s: blocked=%v, want %v", amount, got, want)
		}
	}
}

// A15: the adjustment endpoint validates before it touches the database, so
// these run without PostgreSQL. A 503 here would mean a bad request reached
// the repository, which is how a zero-amount or reason-less correction would
// end up as a 500 instead of a named 400.
func TestConsoleAdjustRejectsBadRequests(t *testing.T) {
	handler := newTestConsole("operator-secret").Handler()
	cases := map[string]struct{ body, wants string }{
		"missing id":     {`{"amount":"-1","reason":"over-charged"}`, "idempotency key"},
		"missing reason": {`{"id":"a1","amount":"-1"}`, "reason is required"},
		"blank reason":   {`{"id":"a1","amount":"-1","reason":"   "}`, "reason is required"},
		// Zero is the one amount an adjustment may never be: it would write a
		// journal row that moves nothing, which is noise an auditor has to
		// explain away later.
		"zero amount": {`{"id":"a1","amount":"0","reason":"over-charged"}`, "non-zero"},
		// Caught by contracts.Decimal at decode time rather than by the
		// handler's own check, which is why it names the decimal and not the
		// sign. Asserted so the earlier rejection stays deliberate.
		"not a decimal": {`{"id":"a1","amount":"abc","reason":"over-charged"}`, "invalid decimal"},
		"unknown field": {`{"id":"a1","amount":"-1","reason":"r","note":"x"}`, ""},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			response := consoleRequest(t, handler, http.MethodPost,
				"/v1/billing/wallets/t1/adjust", "operator-secret", test.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400 (body %s)", response.Code, response.Body.String())
			}
			if test.wants != "" && !strings.Contains(response.Body.String(), test.wants) {
				t.Errorf("error %s does not mention %q", response.Body.String(), test.wants)
			}
		})
	}
}

// Both signs must be accepted: correcting an over-charge refunds, correcting
// an under-charge (B4 §D6's late price entry) debits. A top-up can only ever
// do the first, which is why it could not stand in for this endpoint.
func TestConsoleAdjustAcceptsBothSigns(t *testing.T) {
	handler := newTestConsole("operator-secret").Handler()
	for _, amount := range []string{"-1.25", "1.25"} {
		response := consoleRequest(t, handler, http.MethodPost, "/v1/billing/wallets/t1/adjust",
			"operator-secret", `{"id":"a1","amount":"`+amount+`","reason":"incident refund"}`)
		// No database is configured, so a request that passes validation stops
		// at 503. Anything else means validation rejected a legal amount.
		if response.Code != http.StatusServiceUnavailable {
			t.Errorf("amount %s: got %d, want 503 (body %s)", amount, response.Code, response.Body.String())
		}
	}
}

// A20: an amount the money columns cannot hold is a bad request, not a broken
// platform. Both endpoints had the same gap: the handler checked only format
// and sign, so an over-precise or over-large value reached PostgreSQL and came
// back as a 500. These run without a database precisely because the rejection
// must happen before one is needed.
func TestConsoleWalletRejectsUnstorableAmountsWith400(t *testing.T) {
	handler := newTestConsole("operator-secret").Handler()
	cases := map[string]struct{ target, body, wants string }{
		"adjust: 13 decimal places": {
			"/v1/billing/wallets/t1/adjust",
			`{"id":"a1","amount":"-0.0000000000001","reason":"r"}`,
			"decimal places",
		},
		"adjust: 31 integer digits": {
			"/v1/billing/wallets/t1/adjust",
			`{"id":"a1","amount":"1e30","reason":"r"}`,
			"integer digits",
		},
		"topup: 13 decimal places": {
			"/v1/billing/wallets/t1/topup",
			`{"id":"t1","amount":"0.0000000000001"}`,
			"decimal places",
		},
		"topup: 31 integer digits": {
			"/v1/billing/wallets/t1/topup",
			`{"id":"t1","amount":"1e30"}`,
			"integer digits",
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			response := consoleRequest(t, handler, http.MethodPost, test.target, "operator-secret", test.body)
			// 503 here would mean it reached the "no database" check, i.e. the
			// validation did not happen in the handler at all.
			if response.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400 (body %s)", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), test.wants) {
				t.Errorf("error %s does not name %q", response.Body.String(), test.wants)
			}
		})
	}
}

// The ceiling itself must still be accepted, or the fix above would quietly
// narrow what the platform can record.
func TestConsoleWalletAcceptsAmountsAtTheCeiling(t *testing.T) {
	handler := newTestConsole("operator-secret").Handler()
	for _, amount := range []string{"99999999999999999999999999", "0.000000000001"} {
		response := consoleRequest(t, handler, http.MethodPost, "/v1/billing/wallets/t1/topup",
			"operator-secret", `{"id":"t1","amount":"`+amount+`"}`)
		// No database configured, so a legal amount stops at 503.
		if response.Code != http.StatusServiceUnavailable {
			t.Errorf("amount %s: got %d, want 503 (body %s)", amount, response.Code, response.Body.String())
		}
	}
}

func TestAdjustmentNoteCarriesOperatorAndReason(t *testing.T) {
	note := adjustmentNote("alice", "  refund for incident 42  ")
	if !strings.Contains(note, "alice") || !strings.Contains(note, "refund for incident 42") {
		t.Fatalf("note %q lost the operator or the reason", note)
	}
	// wallet_transactions has no operator column by design (B4.5 reconciles
	// that table), so the note is the only attribution channel there is.
	if strings.Contains(note, "  ") {
		t.Errorf("note %q kept the caller's padding", note)
	}
}

func errorClassFor(succeeded bool) contracts.ErrorClass {
	if succeeded {
		return contracts.ErrorOK
	}
	return contracts.ErrorUpstream5xx
}

// A22: a held row resolved to `unpriced` is revenue that was served and can
// never be collected. It happens rarely enough that the increment creating the
// series is the whole signal ConsoleHeldUnpricedResolution watches, and
// increase() cannot see that increment unless the series starts at zero.
func TestConsolePrimeMetricsCreatesZeroHeldUnpricedSeries(t *testing.T) {
	metrics := observability.NewRegistry()
	console := Console{Metrics: metrics}

	if before := scrape(t, metrics); strings.Contains(before, "control_console_held_unpriced_total") {
		t.Fatalf("the counter existed before priming: %s", before)
	}

	console.PrimeMetrics()
	if want := "control_console_held_unpriced_total 0"; !strings.Contains(scrape(t, metrics), want) {
		t.Errorf("missing primed series %s; got:\n%s", want, scrape(t, metrics))
	}
}
