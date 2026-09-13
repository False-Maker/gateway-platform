package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/elucid/gateway-platform/internal/detail"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

// B5 console integration: the three write paths and the read paths they feed,
// against the real PostgreSQL the rest of the gated suite uses.
//
// The console fixtures live in 2022, for the same reason B4.5's live in 2021:
// this suite shares one database, and every test that asserts over a whole
// window needs its own era so another test's rows cannot wander into the
// assertion.
var (
	b5OccurredAt = time.Date(2022, 3, 7, 9, 30, 0, 0, time.UTC)
)

// newConsoleTestConsole opens the gated database and returns a console wired to
// it. It skips rather than fails when the environment is absent, the way every
// other gated suite here does.
func newConsoleTestConsole(t *testing.T) (context.Context, *pgxpool.Pool, Console) {
	t.Helper()
	ctx, db := openBillingTestDB(t)
	return ctx, db, Console{Auth: ConsoleAuth{Token: "integration-operator"}, DB: db}
}

// b5Fixture seeds one tenant with a wallet, a price, and one held ledger row,
// and returns the tenant and event ids. Cleanup is registered, so each test
// starts from a known state in the shared database.
func b5Fixture(t *testing.T, ctx context.Context, db *pgxpool.Pool, label string) (tenantID, eventID string) {
	t.Helper()
	tenantID = fmt.Sprintf("b5-%s-%d", label, rand.Int63())
	eventID = fmt.Sprintf("b5-event-%s-%d", label, rand.Int63())

	cleanup := func() {
		background := context.Background()
		db.Exec(background, `DELETE FROM billing_resolutions WHERE tenant_id=$1`, tenantID)
		db.Exec(background, `DELETE FROM usage_ledger WHERE tenant_id=$1`, tenantID)
		db.Exec(background, `DELETE FROM wallet_transactions WHERE tenant_id=$1`, tenantID)
		db.Exec(background, `DELETE FROM tenant_wallets WHERE tenant_id=$1`, tenantID)
		db.Exec(background, `DELETE FROM tenants WHERE id=$1`, tenantID)
		db.Exec(background, `ALTER TABLE model_prices DISABLE TRIGGER model_prices_immutable_trigger`)
		db.Exec(background, `DELETE FROM model_prices WHERE id LIKE 'b5-%'`)
		db.Exec(background, `ALTER TABLE model_prices ENABLE TRIGGER model_prices_immutable_trigger`)
	}
	cleanup()
	t.Cleanup(cleanup)

	if _, err := db.Exec(ctx, `INSERT INTO tenants (id,name) VALUES ($1,'B5 console fixture')`, tenantID); err != nil {
		t.Fatal(err)
	}
	// A price that was already in effect when the row occurred: a manual bill
	// is only possible when a price existed at that instant, because D3 forbids
	// backdating one.
	repository := PGBillingRepository{DB: db}
	if err := repository.InsertModelPrice(ctx, ModelPrice{
		ID: "b5-price-" + tenantID, Provider: "b5-provider", Model: "b5-model", Currency: "USD", UnitScale: 1000000,
		PriceInput: "3", PriceOutput: "15", PriceCacheRead: "0.3", PriceCacheWrite: "3.75",
		EffectiveFrom: b5OccurredAt.Add(-30 * 24 * time.Hour),
	}, b5OccurredAt.Add(-60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := repository.EnsureWallet(ctx, tenantID, "USD"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ApplyMovement(ctx, WalletMovement{
		ID: "b5-topup-" + tenantID, TenantID: tenantID, Kind: "topup", Amount: "10",
	}); err != nil {
		t.Fatal(err)
	}
	// Held, with the usage_source that puts it there: a served request whose
	// token counts went missing. The counts are present anyway, which is the
	// case B4.3 said a human has to rule on.
	if _, err := db.Exec(ctx, `
		INSERT INTO usage_ledger (event_id,attempt_id,request_id,tenant_id,account_id,provider,model,
			status_code,error_class,tokens_in,tokens_out,cache_read_tokens,cache_write_tokens,
			usage_source,partial,occurred_at,billing_state)
		VALUES ($1,$2,$3,$4,'acct-b5','b5-provider','b5-model',200,'ok',1000,2000,0,0,
			'missing',false,$5,'held')`,
		eventID, "attempt-"+eventID, "req-"+eventID, tenantID, b5OccurredAt); err != nil {
		t.Fatal(err)
	}
	return tenantID, eventID
}

func b5Request(t *testing.T, console Console, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+console.Auth.Token)
	request.Header.Set("X-Operator", "b5-test-operator")
	recorder := httptest.NewRecorder()
	console.Handler().ServeHTTP(recorder, request)
	return recorder
}

// A held row resolved as `bill` must move the money, mark the ledger, and leave
// a resolution behind -- and B4.5 must still balance afterwards, which is the
// property that makes a manual bill safe to have at all.
func TestConsoleResolveHeldBillsARowAndReconciles(t *testing.T) {
	ctx, db, console := newConsoleTestConsole(t)
	tenantID, eventID := b5Fixture(t, ctx, db, "bill")

	before, err := (Reconciliation{DB: db}).Run(ctx, b5OccurredAt.Add(-time.Hour), b5OccurredAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !before.Balanced() {
		t.Fatalf("fixture starts unbalanced: %+v", before.Discrepancies)
	}

	response := b5Request(t, console, http.MethodPost, "/v1/billing/held/"+eventID+"/resolve", `{"decision":"bill","note":"counts look right"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("resolve: got %d (%s)", response.Code, response.Body.String())
	}
	var resolved struct {
		Outcome    string `json:"outcome"`
		Resolution struct {
			Amount       string `json:"amount"`
			BillingRunID string `json:"billing_run_id"`
			Operator     string `json:"operator"`
		} `json:"resolution"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &resolved); err != nil {
		t.Fatal(err)
	}
	if resolved.Outcome != "billed" {
		t.Fatalf("outcome %q, want billed", resolved.Outcome)
	}
	// 1000 input tokens at 3/1M plus 2000 output at 15/1M = 0.003 + 0.03.
	if resolved.Resolution.Amount != "0.033000000000" {
		t.Errorf("amount %q, want 0.033000000000", resolved.Resolution.Amount)
	}
	if resolved.Resolution.Operator != "b5-test-operator" {
		t.Errorf("operator %q, want the X-Operator claim", resolved.Resolution.Operator)
	}

	// The wallet moved by exactly that amount.
	balance, exists, err := (PGBillingRepository{DB: db}).Balance(ctx, tenantID)
	if err != nil || !exists {
		t.Fatalf("read balance: %v (exists=%v)", err, exists)
	}
	if balance != "9.967000000000" {
		t.Errorf("balance %s, want 9.967000000000", balance)
	}

	// The ledger row is billed and carries the same run id as the debit, which
	// is the whole reason a manual bill reconciles like a job-settled one.
	var state, billedAmount, runID string
	if err := db.QueryRow(ctx, `
		SELECT billing_state, billed_amount::text, billing_run_id FROM usage_ledger WHERE event_id=$1`,
		eventID).Scan(&state, &billedAmount, &runID); err != nil {
		t.Fatal(err)
	}
	if state != "billed" || billedAmount != "0.033000000000" || runID != resolved.Resolution.BillingRunID {
		t.Errorf("ledger row: state=%s amount=%s run=%s", state, billedAmount, runID)
	}
	var debitRun string
	if err := db.QueryRow(ctx, `
		SELECT billing_run_id FROM wallet_transactions WHERE tenant_id=$1 AND kind='debit'`,
		tenantID).Scan(&debitRun); err != nil {
		t.Fatal(err)
	}
	if debitRun != runID {
		t.Errorf("wallet debit carries run %q, ledger row carries %q", debitRun, runID)
	}

	after, err := (Reconciliation{DB: db}).Run(ctx, b5OccurredAt.Add(-time.Hour), b5OccurredAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !after.Balanced() {
		t.Fatalf("reconciliation is unbalanced after a manual bill: %+v", after.Discrepancies)
	}
	if after.TotalBilled != "0.033000000000" {
		t.Errorf("window billed total %s, want 0.033000000000", after.TotalBilled)
	}
}

// A write-off is a decision, not an absence of one: the row must end in
// not_billable, no money may move, and the ruling must be recorded.
func TestConsoleResolveHeldWritesOffWithoutMovingMoney(t *testing.T) {
	ctx, db, console := newConsoleTestConsole(t)
	tenantID, eventID := b5Fixture(t, ctx, db, "writeoff")

	response := b5Request(t, console, http.MethodPost, "/v1/billing/held/"+eventID+"/resolve", `{"decision":"write_off"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("resolve: got %d (%s)", response.Code, response.Body.String())
	}
	var state string
	var amount *string
	if err := db.QueryRow(ctx, `SELECT billing_state, billed_amount::text FROM usage_ledger WHERE event_id=$1`, eventID).
		Scan(&state, &amount); err != nil {
		t.Fatal(err)
	}
	if state != "not_billable" || amount != nil {
		t.Errorf("row is %s with amount %v, want not_billable with NULL", state, amount)
	}
	balance, _, err := (PGBillingRepository{DB: db}).Balance(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if balance != "10.000000000000" {
		t.Errorf("write-off moved money: balance %s", balance)
	}
	var decision string
	if err := db.QueryRow(ctx, `SELECT decision FROM billing_resolutions WHERE event_id=$1`, eventID).Scan(&decision); err != nil {
		t.Fatal(err)
	}
	if decision != "write_off" {
		t.Errorf("recorded decision %q, want write_off", decision)
	}
}

// A held row with no price in effect cannot be billed -- D3 has no mechanism
// for pricing the past -- so the honest outcome is the terminal `unpriced`
// state. Refusing the request would leave the operator stuck; silently charging
// zero would be the implicit default B4.0 forbade.
func TestConsoleResolveHeldWithoutAPriceBecomesUnpriced(t *testing.T) {
	ctx, db, console := newConsoleTestConsole(t)
	tenantID, eventID := b5Fixture(t, ctx, db, "unpriced")
	// Move the row into an era no price covers.
	if _, err := db.Exec(ctx, `UPDATE usage_ledger SET occurred_at=$2 WHERE event_id=$1`, eventID,
		time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}

	response := b5Request(t, console, http.MethodPost, "/v1/billing/held/"+eventID+"/resolve", `{"decision":"bill"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("resolve: got %d (%s)", response.Code, response.Body.String())
	}
	var resolution struct {
		Outcome    string `json:"outcome"`
		Resolution struct {
			Amount string `json:"amount"`
		} `json:"resolution"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &resolution); err != nil {
		t.Fatal(err)
	}
	if resolution.Outcome != "unpriced" {
		t.Fatalf("outcome %q, want unpriced", resolution.Outcome)
	}
	var state string
	if err := db.QueryRow(ctx, `SELECT billing_state FROM usage_ledger WHERE event_id=$1`, eventID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "unpriced" {
		t.Errorf("row is %s, want unpriced", state)
	}
	balance, _, err := (PGBillingRepository{DB: db}).Balance(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if balance != "10.000000000000" {
		t.Errorf("an unpriced resolution moved money: balance %s", balance)
	}
}

// Resolving the same row twice must not bill it twice: the second call sees a
// row that is no longer held and conflicts.
func TestConsoleResolveHeldIsNotRepeatable(t *testing.T) {
	ctx, db, console := newConsoleTestConsole(t)
	_, eventID := b5Fixture(t, ctx, db, "twice")

	first := b5Request(t, console, http.MethodPost, "/v1/billing/held/"+eventID+"/resolve", `{"decision":"bill"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first resolve: got %d (%s)", first.Code, first.Body.String())
	}
	second := b5Request(t, console, http.MethodPost, "/v1/billing/held/"+eventID+"/resolve", `{"decision":"write_off"}`)
	if second.Code != http.StatusConflict {
		t.Fatalf("second resolve: got %d, want 409 (%s)", second.Code, second.Body.String())
	}
	var resolutions int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM billing_resolutions WHERE event_id=$1`, eventID).Scan(&resolutions); err != nil {
		t.Fatal(err)
	}
	if resolutions != 1 {
		t.Errorf("recorded %d resolutions for one row, want 1", resolutions)
	}
}

// An unknown event id is a 404, not a 409: the distinction is what tells an
// operator whether they mistyped and whether someone else got there first.
func TestConsoleResolveHeldSeparatesNotFoundFromConflict(t *testing.T) {
	_, _, console := newConsoleTestConsole(t)
	response := b5Request(t, console, http.MethodPost, "/v1/billing/held/does-not-exist/resolve", `{"decision":"write_off"}`)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown event: got %d, want 404 (%s)", response.Code, response.Body.String())
	}
}

// The tray lists held rows with the counts and the reason, so an operator can
// decide without a second query.
func TestConsoleHeldTrayListsRowsWithTheirReason(t *testing.T) {
	ctx, db, console := newConsoleTestConsole(t)
	tenantID, eventID := b5Fixture(t, ctx, db, "tray")

	response := b5Request(t, console, http.MethodGet, "/v1/billing/held?tenant="+tenantID, "")
	if response.Code != http.StatusOK {
		t.Fatalf("held tray: got %d (%s)", response.Code, response.Body.String())
	}
	var tray struct {
		Held []heldRow `json:"held"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &tray); err != nil {
		t.Fatal(err)
	}
	if len(tray.Held) != 1 {
		t.Fatalf("tray holds %d rows, want 1", len(tray.Held))
	}
	row := tray.Held[0]
	if row.EventID != eventID || row.TokensIn != 1000 || row.TokensOut != 2000 {
		t.Errorf("tray row %+v does not match the fixture", row)
	}
	if !strings.Contains(row.HeldBecause, "never reported token counts") {
		t.Errorf("held_because %q does not explain the hold", row.HeldBecause)
	}
}

// A top-up credits the wallet, creates it if absent, and a repeated id returns
// the original movement instead of crediting twice.
func TestConsoleTopupIsIdempotent(t *testing.T) {
	ctx, db, console := newConsoleTestConsole(t)
	tenantID := fmt.Sprintf("b5-topup-%d", rand.Int63())
	t.Cleanup(func() {
		background := context.Background()
		db.Exec(background, `DELETE FROM wallet_transactions WHERE tenant_id=$1`, tenantID)
		db.Exec(background, `DELETE FROM tenant_wallets WHERE tenant_id=$1`, tenantID)
		db.Exec(background, `DELETE FROM tenants WHERE id=$1`, tenantID)
	})
	if _, err := db.Exec(ctx, `INSERT INTO tenants (id,name) VALUES ($1,'B5 topup fixture')`, tenantID); err != nil {
		t.Fatal(err)
	}

	body := `{"id":"` + tenantID + `-topup","amount":"25.5","note":"invoice 42"}`
	first := b5Request(t, console, http.MethodPost, "/v1/billing/wallets/"+tenantID+"/topup", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first top-up: got %d (%s)", first.Code, first.Body.String())
	}
	second := b5Request(t, console, http.MethodPost, "/v1/billing/wallets/"+tenantID+"/topup", body)
	if second.Code != http.StatusOK {
		t.Fatalf("replayed top-up: got %d (%s)", second.Code, second.Body.String())
	}
	var replay struct {
		Replayed bool `json:"replayed"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed {
		t.Error("a repeated id was not reported as a replay")
	}

	balance, exists, err := (PGBillingRepository{DB: db}).Balance(ctx, tenantID)
	if err != nil || !exists {
		t.Fatalf("read balance: %v (exists=%v)", err, exists)
	}
	if balance != "25.500000000000" {
		t.Errorf("balance %s after one top-up applied twice, want 25.500000000000", balance)
	}

	// The operator is attributed in the journal, since that is the record that
	// travels with the money.
	var note string
	if err := db.QueryRow(ctx, `SELECT note FROM wallet_transactions WHERE id=$1`, tenantID+"-topup").Scan(&note); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "b5-test-operator") || !strings.Contains(note, "invoice 42") {
		t.Errorf("journal note %q does not carry the operator and the operator's note", note)
	}
}

// The wallet read reports the same exhaustion verdict B4.4 publishes, so the
// console cannot tell an operator a tenant is fine while the gateway 402s it.
func TestConsoleWalletReportsTheSameBlockedVerdict(t *testing.T) {
	ctx, db, console := newConsoleTestConsole(t)
	tenantID, _ := b5Fixture(t, ctx, db, "wallet")

	response := b5Request(t, console, http.MethodGet, "/v1/billing/wallets/"+tenantID, "")
	if response.Code != http.StatusOK {
		t.Fatalf("wallet: got %d (%s)", response.Code, response.Body.String())
	}
	var body struct {
		Wallet  walletView `json:"wallet"`
		Blocked bool       `json:"blocked"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Blocked {
		t.Error("a funded tenant is reported as blocked")
	}
	if body.Wallet.Balance != "10.000000000000" {
		t.Errorf("balance %s, want 10.000000000000", body.Wallet.Balance)
	}

	// Drain it and the verdict must flip, with no threshold of its own.
	if _, err := db.Exec(ctx, `UPDATE tenant_wallets SET balance=0 WHERE tenant_id=$1`, tenantID); err != nil {
		t.Fatal(err)
	}
	response = b5Request(t, console, http.MethodGet, "/v1/billing/wallets/"+tenantID, "")
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Blocked {
		t.Error("a drained wallet is not reported as blocked, so the console disagrees with B4.4")
	}
}

// A tenant with no wallet row is not blocked -- that is B4.4's rule, and the
// console must not invent a stricter one.
func TestConsoleWalletWithNoRowIsNotBlocked(t *testing.T) {
	_, _, console := newConsoleTestConsole(t)
	response := b5Request(t, console, http.MethodGet, "/v1/billing/wallets/b5-no-such-tenant", "")
	if response.Code != http.StatusOK {
		t.Fatalf("wallet: got %d (%s)", response.Code, response.Body.String())
	}
	var body struct {
		Wallet  *walletView `json:"wallet"`
		Blocked bool        `json:"blocked"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Wallet != nil || body.Blocked {
		t.Errorf("absent wallet reported as %+v blocked=%v", body.Wallet, body.Blocked)
	}
}

// The price endpoint appends a row and the table stays immutable: a second
// insert of the same (provider, model, effective_from) is a conflict, and
// nothing can update the first row.
func TestConsolePriceInsertAppendsAndCannotRewriteHistory(t *testing.T) {
	ctx, db, console := newConsoleTestConsole(t)
	provider := fmt.Sprintf("b5-price-provider-%d", rand.Int63())
	model := "b5-price-model"
	t.Cleanup(func() {
		background := context.Background()
		db.Exec(background, `ALTER TABLE model_prices DISABLE TRIGGER model_prices_immutable_trigger`)
		db.Exec(background, `DELETE FROM model_prices WHERE provider=$1`, provider)
		db.Exec(background, `ALTER TABLE model_prices ENABLE TRIGGER model_prices_immutable_trigger`)
	})

	effectiveFrom := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	body := `{"id":"` + provider + `","provider":"` + provider + `","model":"` + model + `","unit_scale":1000000,` +
		`"price_input":"3","price_output":"15","price_cache_read":"0.3","price_cache_write":"3.75",` +
		`"effective_from":"` + effectiveFrom + `"}`
	first := b5Request(t, console, http.MethodPost, "/v1/billing/prices", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("insert price: got %d (%s)", first.Code, first.Body.String())
	}
	// Same effective_from, different id: still a conflict, because the table's
	// uniqueness is on (provider, model, effective_from), not on the id.
	duplicate := strings.Replace(body, `"id":"`+provider+`"`, `"id":"`+provider+`-second"`, 1)
	second := b5Request(t, console, http.MethodPost, "/v1/billing/prices", duplicate)
	if second.Code != http.StatusConflict {
		t.Fatalf("duplicate effective_from: got %d, want 409 (%s)", second.Code, second.Body.String())
	}
	// And the immutability trigger still refuses an update, so the console
	// cannot be the loophole that rewrites a past price.
	if _, err := db.Exec(ctx, `UPDATE model_prices SET price_input=99 WHERE provider=$1`, provider); err == nil {
		t.Error("a model_price row was updated; the immutability trigger did not fire")
	}

	list := b5Request(t, console, http.MethodGet, "/v1/billing/prices?provider="+provider, "")
	if list.Code != http.StatusOK {
		t.Fatalf("list prices: got %d (%s)", list.Code, list.Body.String())
	}
	var prices struct {
		Prices []priceView `json:"prices"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &prices); err != nil {
		t.Fatal(err)
	}
	if len(prices.Prices) != 1 || prices.Prices[0].Model != model {
		t.Errorf("price list %+v does not contain the inserted row", prices.Prices)
	}
}

// A held row worth nothing is still settled, but writes no wallet movement: a
// zero-amount journal entry would be noise in B4.5, and the billing job omits
// it for the same reason. The run still reconciles, because a run with no
// debit is compared against a zero debit rather than a missing one.
func TestConsoleResolveHeldZeroAmountWritesNoJournalEntry(t *testing.T) {
	ctx, db, console := newConsoleTestConsole(t)
	tenantID, eventID := b5Fixture(t, ctx, db, "zero")
	if _, err := db.Exec(ctx, `
		UPDATE usage_ledger SET tokens_in=0,tokens_out=0,cache_read_tokens=0,cache_write_tokens=0
		WHERE event_id=$1`, eventID); err != nil {
		t.Fatal(err)
	}

	response := b5Request(t, console, http.MethodPost, "/v1/billing/held/"+eventID+"/resolve", `{"decision":"bill"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("resolve: got %d (%s)", response.Code, response.Body.String())
	}
	var state, amount string
	if err := db.QueryRow(ctx, `SELECT billing_state, billed_amount::text FROM usage_ledger WHERE event_id=$1`, eventID).
		Scan(&state, &amount); err != nil {
		t.Fatal(err)
	}
	if state != "billed" || amount != "0.000000000000" {
		t.Errorf("row is %s with amount %s, want billed with an explicit zero", state, amount)
	}
	var debits int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM wallet_transactions WHERE tenant_id=$1 AND kind='debit'`, tenantID).Scan(&debits); err != nil {
		t.Fatal(err)
	}
	if debits != 0 {
		t.Errorf("a zero-amount settlement wrote %d journal entries, want 0", debits)
	}
	report, err := (Reconciliation{DB: db}).Run(ctx, b5OccurredAt.Add(-time.Hour), b5OccurredAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !report.Balanced() {
		t.Errorf("a zero-amount settlement unbalanced the reconciliation: %+v", report.Discrepancies)
	}
}

// A top-up aimed at a tenant that does not exist is a 404, not a 500: the
// console is working, the request named something that is not there.
func TestConsoleTopupUnknownTenantIsNotFound(t *testing.T) {
	_, _, console := newConsoleTestConsole(t)
	response := b5Request(t, console, http.MethodPost, "/v1/billing/wallets/b5-no-such-tenant/topup",
		`{"id":"b5-unknown-topup","amount":"10"}`)
	if response.Code != http.StatusNotFound {
		t.Fatalf("top-up for an unknown tenant: got %d, want 404 (%s)", response.Code, response.Body.String())
	}
}

// b5DetailStore returns a writer against the test ClickHouse, or skips.
func b5DetailStore(t *testing.T) (context.Context, detail.ClickHouseWriter, string) {
	t.Helper()
	endpoint := os.Getenv("GATEWAY_TEST_CLICKHOUSE_URL")
	if endpoint == "" {
		t.Skip("GATEWAY_TEST_CLICKHOUSE_URL is not set")
	}
	ctx := context.Background()
	writer := detail.ClickHouseWriter{URL: endpoint, Database: "gateway_detail_test", Table: "request_detail"}
	if err := writer.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	return ctx, writer, endpoint
}

// b5DeleteDetail removes this test's rows. It talks to ClickHouse over plain
// HTTP rather than through the detail package, because the package exposes no
// delete and must not grow one for a test's sake: nothing in production ever
// deletes a detail row, the TTL does.
func b5DeleteDetail(t *testing.T, endpoint, tenantID string) {
	t.Helper()
	statement := "ALTER TABLE gateway_detail_test.request_detail DELETE WHERE tenant_id={tenant:String}"
	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	user := target.User
	target.User = nil
	parameters := target.Query()
	parameters.Set("query", statement)
	parameters.Set("param_tenant", tenantID)
	target.RawQuery = parameters.Encode()
	request, err := http.NewRequest(http.MethodPost, target.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if user != nil {
		password, _ := user.Password()
		request.SetBasicAuth(user.Username(), password)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	io.Copy(io.Discard, response.Body)
}

// b5DetailRelease is the Release the detail row is projected from. It is built
// here rather than borrowed from the detail package's own test fixtures, which
// are unexported and belong to that package's tests.
func b5DetailRelease(tenantID string) contracts.Release {
	attemptID := "b5-console-attempt"
	return contracts.Release{
		SchemaVersion: contracts.SchemaVersion, EventID: contracts.TerminalEventID(attemptID),
		RequestID: "b5-console-req", AttemptID: attemptID, AttemptNo: 2,
		ProducerID: "gw-node-b5", OccurredAt: time.Date(2022, 3, 7, 9, 30, 0, 0, time.UTC),
		AccountID: "acct-b5", Provider: "b5-detail-provider", Model: "b5-model", TenantID: tenantID,
		StatusCode: 200, LatencyMS: 1234, ErrorClass: contracts.ErrorOK,
		UsageSource: contracts.UsageSourceUpstream, Partial: false,
		TokensIn: 11, TokensOut: 22, CacheReadTokens: 33, CacheWriteTokens: 44,
		InboundProtocol: "openai_chat",
	}
}

// The request log reads the detail store, filters by tenant, and reports the
// count it actually returned. Detail is not the ledger, so the endpoint returns
// rows and never a sum.
func TestConsoleRequestLogReadsDetail(t *testing.T) {
	ctx, writer, endpoint := b5DetailStore(t)
	tenantID := fmt.Sprintf("b5-detail-%d", rand.Int63())
	b5DeleteDetail(t, endpoint, tenantID)
	t.Cleanup(func() { b5DeleteDetail(t, endpoint, tenantID) })

	if err := writer.WriteBatch(ctx, []detail.Record{detail.FromRelease(b5DetailRelease(tenantID), time.Now().UTC())}); err != nil {
		t.Fatal(err)
	}

	_, _, console := newConsoleTestConsole(t)
	console.Detail = writer
	response := b5Request(t, console, http.MethodGet, "/v1/requests?tenant="+tenantID+"&provider=b5-detail-provider", "")
	if response.Code != http.StatusOK {
		t.Fatalf("request log: got %d (%s)", response.Code, response.Body.String())
	}
	var log struct {
		Requests []detail.Record `json:"requests"`
		Count    int             `json:"count"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &log); err != nil {
		t.Fatal(err)
	}
	if log.Count != 1 || len(log.Requests) != 1 {
		t.Fatalf("got %d detail rows, want 1 (response %s)", log.Count, response.Body.String())
	}
	record := log.Requests[0]
	if record.TenantID != tenantID || record.Provider != "b5-detail-provider" || record.TokensIn != 11 {
		t.Errorf("detail record %+v does not match what was written", record)
	}
	// A tenant filter that matches nothing is an empty list, not an error: the
	// caller has to be able to tell "no traffic" from "no detail store", which
	// is the 503 the disabled case returns.
	empty := b5Request(t, console, http.MethodGet, "/v1/requests?tenant=b5-nobody", "")
	if empty.Code != http.StatusOK {
		t.Fatalf("empty result: got %d, want 200", empty.Code)
	}
	if err := json.Unmarshal(empty.Body.Bytes(), &log); err != nil {
		t.Fatal(err)
	}
	if log.Count != 0 {
		t.Errorf("filter matched %d rows for a tenant that has none", log.Count)
	}
}

// A tenant id is a bound parameter, never spliced into SQL. The proof is that a
// value full of quotes and a DROP returns cleanly with no rows instead of
// erroring or executing.
func TestConsoleRequestLogBindsTheTenantFilter(t *testing.T) {
	_, writer, _ := b5DetailStore(t)
	_, _, console := newConsoleTestConsole(t)
	console.Detail = writer

	hostile := url.QueryEscape(`' OR 1=1; DROP TABLE gateway_detail_test.request_detail; --`)
	response := b5Request(t, console, http.MethodGet, "/v1/requests?tenant="+hostile, "")
	if response.Code != http.StatusOK {
		t.Fatalf("hostile tenant filter: got %d (%s)", response.Code, response.Body.String())
	}
	var log struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &log); err != nil {
		t.Fatal(err)
	}
	if log.Count != 0 {
		t.Errorf("a hostile tenant filter matched %d rows", log.Count)
	}
	// The table is still there, which it would not be if the value had been
	// concatenated into the statement.
	if err := writer.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Query(context.Background(), detail.QueryFilter{Limit: 1}); err != nil {
		t.Fatalf("detail table is unusable after a hostile filter: %v", err)
	}
}

func TestConsoleAccountsBoardScansRealRows(t *testing.T) {
	ctx, db, console := newConsoleTestConsole(t)
	// A9 columns, including a cooldown and an excluded model.
	if _, err := db.Exec(ctx, `
		INSERT INTO accounts (id,provider,platform,"group",source_system,source_id,status,
			last_error_class,last_error_at,consecutive_failures,cooldown_until,excluded_models)
		VALUES ('b5-acct-1','b5-prov','b5-plat','default','b5','b5-acct-1','active',
			'rate_limited_unknown', now(), 3, now() + interval '5 minutes', '["b5-model"]'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Exec(context.Background(), `DELETE FROM accounts WHERE id='b5-acct-1'`)
	})

	response := b5Request(t, console, http.MethodGet, "/v1/accounts?provider=b5-prov", "")
	if response.Code != http.StatusOK {
		t.Fatalf("accounts: got %d (%s)", response.Code, response.Body.String())
	}
	var body struct {
		Accounts []accountView `json:"accounts"`
		Count    int           `json:"count"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Count != 1 {
		t.Fatalf("got %d accounts, want 1 (%s)", body.Count, response.Body.String())
	}
	account := body.Accounts[0]
	// The credential-ish columns must not appear anywhere in the response, so
	// this asserts on the raw body rather than on the struct: a field added to
	// accountView later would otherwise slip past it.
	for _, forbidden := range []string{"credential_id", "base_url", "proxy", "model_mapping"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Errorf("account board exposes %q", forbidden)
		}
	}
	if account.LastErrorClass != "rate_limited_unknown" || account.ConsecutiveFailures != 3 {
		t.Errorf("account %+v did not round-trip the A9 columns", account)
	}
	if account.CooldownUntil == "" || account.LastErrorAt == "" || account.UpdatedAt == "" {
		t.Errorf("timestamps were not rendered: %+v", account)
	}
	if len(account.ExcludedModels) != 1 || account.ExcludedModels[0] != "b5-model" {
		t.Errorf("excluded_models = %v", account.ExcludedModels)
	}
}
