package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

// deadPool is a pool pointed at a port nothing listens on. It stands in for
// every way the database can be unreachable at the moment a handler needs it.
func deadPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/nope")
	if err != nil {
		t.Fatalf("dead pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func consoleWithDeadDB(t *testing.T) http.Handler {
	t.Helper()
	return Console{Auth: ConsoleAuth{Token: "operator-secret"}, DB: deadPool(t), Now: func() time.Time {
		return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	}}.Handler()
}

const validPriceBody = `{"id":"p1","provider":"anthropic","model":"claude-opus-5","unit_scale":1000000,` +
	`"price_input":"3","price_output":"15","price_cache_read":"0.3","price_cache_write":"3.75",` +
	`"effective_from":"2026-10-01T00:00:00Z"}`

// A29: the price endpoint used to answer every repository failure with a 400
// carrying the driver's own error text. An unreachable database told the
// operator their input was wrong, kept a platform outage out of every 5xx
// signal, and printed the database user, database name and address into a
// response body.
func TestPriceEndpointReportsDatabaseFailureAsServerError(t *testing.T) {
	response := consoleRequest(t, consoleWithDeadDB(t), http.MethodPost, "/v1/billing/prices",
		"operator-secret", validPriceBody)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("unreachable database: got %d, want 500 (%s)", response.Code, response.Body.String())
	}
	body := response.Body.String()
	// Console.fail's whole contract: the detail goes to the log, not the wire.
	for _, leak := range []string{"127.0.0.1", "database=", "user=", "connection refused", "dial"} {
		if strings.Contains(body, leak) {
			t.Errorf("response leaks driver detail %q: %s", leak, body)
		}
	}
}

// The reverse of the above: the genuine 400s must survive the change, or A29
// would have fixed the misreport by making every rejection unreadable.
func TestPriceEndpointStillRejectsBadInputWithFourHundred(t *testing.T) {
	handler := consoleWithDeadDB(t)
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unit_scale must be positive",
			body: `{"id":"p1","provider":"anthropic","model":"m","unit_scale":0,` +
				`"price_input":"3","price_output":"15","price_cache_read":"0.3","price_cache_write":"3.75",` +
				`"effective_from":"2026-10-01T00:00:00Z"}`,
			want: "unit_scale",
		},
		{
			name: "a negative price is not a price",
			body: `{"id":"p1","provider":"anthropic","model":"m","unit_scale":1000000,` +
				`"price_input":"-3","price_output":"15","price_cache_read":"0.3","price_cache_write":"3.75",` +
				`"effective_from":"2026-10-01T00:00:00Z"}`,
			want: "negative",
		},
		{
			name: "an amount the money columns cannot hold",
			body: `{"id":"p1","provider":"anthropic","model":"m","unit_scale":1000000,` +
				`"price_input":"1e30","price_output":"15","price_cache_read":"0.3","price_cache_write":"3.75",` +
				`"effective_from":"2026-10-01T00:00:00Z"}`,
			want: "integer digits",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			response := consoleRequest(t, handler, http.MethodPost, "/v1/billing/prices", "operator-secret", test.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400 (%s)", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), test.want) {
				t.Errorf("rejection does not name %q: %s", test.want, response.Body.String())
			}
		})
	}
}

// Every rejection InsertModelPrice raises before it reaches PostgreSQL must be
// reachable as a 400, which means it must carry ErrPriceRejected. Without this
// a new validation added later would default into A29's 500 -- the same
// hand-maintained-list failure F3 of the sixth review recorded.
func TestEveryPriceValidationCarriesTheRejectedSentinel(t *testing.T) {
	repository := PGBillingRepository{DB: deadPool(t)}
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	valid := ModelPrice{
		ID: "p1", Provider: "anthropic", Model: "m", UnitScale: 1000000,
		PriceInput: "3", PriceOutput: "15", PriceCacheRead: "0.3", PriceCacheWrite: "3.75",
		EffectiveFrom: future,
	}
	mutate := map[string]func(p ModelPrice) ModelPrice{
		"missing id":          func(p ModelPrice) ModelPrice { p.ID = ""; return p },
		"missing provider":    func(p ModelPrice) ModelPrice { p.Provider = ""; return p },
		"missing model":       func(p ModelPrice) ModelPrice { p.Model = ""; return p },
		"zero unit_scale":     func(p ModelPrice) ModelPrice { p.UnitScale = 0; return p },
		"negative unit_scale": func(p ModelPrice) ModelPrice { p.UnitScale = -1; return p },
		"too many decimals":   func(p ModelPrice) ModelPrice { p.PriceInput = "0.1234567890123"; return p },
		"too many integers":   func(p ModelPrice) ModelPrice { p.PriceInput = "1e30"; return p },
		"negative price":      func(p ModelPrice) ModelPrice { p.PriceInput = "-1"; return p },
		"unparsable price":    func(p ModelPrice) ModelPrice { p.PriceInput = "abc"; return p },
	}
	for name, apply := range mutate {
		t.Run(name, func(t *testing.T) {
			err := repository.InsertModelPrice(context.Background(), apply(valid), now)
			if err == nil {
				t.Fatalf("%s was accepted", name)
			}
			if !errors.Is(err, ErrPriceRejected) {
				t.Errorf("%s is not marked as the caller's fault, so the handler answers 500: %v", name, err)
			}
		})
	}
	// The retroactive case has its own sentinel and its own 400 branch; assert
	// it stays distinguishable rather than folding into the generic one.
	past := valid
	past.EffectiveFrom = now.Add(-time.Hour)
	if err := repository.InsertModelPrice(context.Background(), past, now); !errors.Is(err, ErrPriceNotEffectiveInFuture) {
		t.Errorf("retroactive price lost its own sentinel: %v", err)
	}
}

// A30: moneyIntegerDigits added the exponent to the integer part's length,
// ignoring the fractional digits a positive exponent consumes. The error only
// ran one way -- nothing unstorable reached PostgreSQL -- but amounts the money
// columns can hold came back as 400s. A20's own reverse guard missed it because
// it only used plain decimals.
func TestMoneyIntegerDigitsCountsExponentForms(t *testing.T) {
	ceiling := moneyPrecision - moneyScale // 26
	cases := []struct {
		amount string
		digits int
	}{
		{"0", 0},
		{"0.5", 0},
		{"1", 1},
		{"-1", 1},
		{"+1", 1},
		{"1.5e3", 4},
		{"1500e-2", 2},
		{"0.001e28", 26},  // exactly 1e25
		{"0.0001e29", 26}, // the same value written differently
		{"1e25", 26},
		{"0.1e26", 26}, // = 1e25
		{"1e26", 27},
		{"0.1e27", 27}, // = 1e26, one digit over
		{"0.001e29", 27},
		{"1e30", 31},
		{"0.5e30", 30},
		{"-0.001e28", 26},
		{"0.000e30", 0}, // every digit is a zero, so the exponent moves nothing
	}
	for _, test := range cases {
		if got := moneyIntegerDigits(contracts.Decimal(test.amount)); got != test.digits {
			t.Errorf("moneyIntegerDigits(%q) = %d, want %d", test.amount, got, test.digits)
		}
	}
	// The boundary itself, stated as acceptance rather than as a digit count:
	// these are storable and checkMoney must let them through however written.
	for _, storable := range []string{"0.001e28", "0.0001e29", "1e25", "0.1e26", "-0.001e28"} {
		if err := checkMoney("amount", contracts.Decimal(storable)); err != nil {
			t.Errorf("checkMoney rejected the storable %q: %v", storable, err)
		}
	}
	for _, unstorable := range []string{"1e26", "0.1e27", "0.001e29", "1e30", "0.5e30"} {
		if err := checkMoney("amount", contracts.Decimal(unstorable)); err == nil {
			t.Errorf("checkMoney accepted %q, which needs more than %d integer digits", unstorable, ceiling)
		}
	}
}

// A31: A18 parks an account whose model list could not be read, because an
// empty capability set means "matches every model" to gateway. The only way
// back is this endpoint, and nothing in control ever writes capabilities -- so
// the single available action restored precisely the state A18 removed.
func TestActivatingAnUndescribedAccountNeedsAcknowledgement(t *testing.T) {
	cases := []struct {
		name         string
		status       string
		capabilities []string
		acknowledged bool
		want         bool
	}{
		{"parked account back to active", "active", nil, false, true},
		{"the same, acknowledged", "active", nil, true, false},
		{"an account that describes models", "active", []string{"claude-opus-5"}, false, false},
		// Taking something out of rotation is never gated: it is how the
		// account got parked, and an incident is the wrong time to argue.
		{"disabling is never gated", "disabled", nil, false, false},
		{"disabling an acknowledged one", "disabled", nil, true, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := needsUnrestrictedAcknowledgement(test.status, test.capabilities, test.acknowledged); got != test.want {
				t.Errorf("needsUnrestrictedAcknowledgement(%q, %v, %v) = %v, want %v",
					test.status, test.capabilities, test.acknowledged, got, test.want)
			}
		})
	}
}

// The board is where an operator decides whether to release a parked account,
// so it has to show the field that decision turns on. Until A31 it carried
// excluded_models but not capabilities, leaving "serves every model" and
// "serves the three it lists" identical on screen.
func TestAccountBoardReportsCapabilities(t *testing.T) {
	var view accountView
	if _, ok := any(view.Capabilities).([]string); !ok {
		t.Fatal("accountView has no capabilities field")
	}
	encoded, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		t.Fatalf("marshal account view: %v", err)
	}
	body := string(encoded)
	if !strings.Contains(body, `"capabilities"`) {
		t.Errorf("capabilities is absent from the board's JSON: %s", body)
	}
	// Not omitempty: an absent key and an empty list would read the same, and
	// the empty list is exactly the state worth seeing.
	if !strings.Contains(body, `"capabilities": null`) && !strings.Contains(body, `"capabilities": []`) {
		t.Errorf("an empty capability set is not visible in the JSON: %s", body)
	}
}
