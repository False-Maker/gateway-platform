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
// The era trick works because those rows live in PostgreSQL, which has no
// retention policy. It must NOT be extended to the ClickHouse detail store:
// internal/detail's table is created with `TTL occurred_at + 30 DAY`, so a row
// dated 2022 is already expired when it is written and any background merge may
// drop it between the write and the read. That is a race, not a failure, which
// is why it survived a full B5 round and only showed up on the 5th full run
// (see docs/EVIDENCE.md, "真实基础设施集成测试复跑").
//
// Detail fixtures therefore use b5DetailOccurredAt below. They do not need an
// era of their own: the detail assertions filter by a random tenant id rather
// than by a time window, so no other test's rows can reach them.
var (
	b5OccurredAt = time.Date(2022, 3, 7, 9, 30, 0, 0, time.UTC)
)

// b5DetailOccurredAt is a detail-store timestamp that is inside the table's TTL
// window. It is computed per call rather than fixed, because any hard-coded
// date eventually falls out of a 30-day window and would reintroduce exactly
// the race it exists to avoid. Truncated to milliseconds to match the column's
// DateTime64(3) so the value round-trips exactly.
func b5DetailOccurredAt() time.Time {
	return time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
}

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
		ProducerID: "gw-node-b5", OccurredAt: b5DetailOccurredAt(),
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

// TestDetailFixtureStaysInsideTheRetentionWindow guards the fix for a race that
// hid in this suite through an entire B5 round: the detail fixture used to be
// dated 2022, which is outside internal/detail's 30-day TTL, so the row was
// already expired when written and any background merge could drop it before
// the assertion read it. The test then passed or failed depending on merge
// timing -- it failed on 1 of 5 full runs and never in isolation.
//
// Needs no infrastructure: it is arithmetic on the fixture, so it also runs in
// the default `go test ./...` where the ClickHouse suites skip. Keeping it
// close to the era convention above is deliberate -- the convention reads like
// it should apply to the detail fixture too, and this is what says it must not.
func TestDetailFixtureStaysInsideTheRetentionWindow(t *testing.T) {
	occurred := b5DetailRelease("any-tenant").OccurredAt
	age := time.Since(occurred)
	if age < 0 {
		t.Fatalf("detail fixture %s is in the future", occurred)
	}
	if ttl := time.Duration(detail.DefaultTTLDays) * 24 * time.Hour; age >= ttl {
		t.Fatalf("detail fixture %s is %s old, at or past the %s TTL: the row can be dropped by a merge before it is read", occurred, age, ttl)
	}
}

// b5Account seeds one account and returns its id. B5.1's three manual actions
// all target this row.
func b5Account(t *testing.T, ctx context.Context, db *pgxpool.Pool, label string) string {
	t.Helper()
	accountID := fmt.Sprintf("b51-%s-%d", label, rand.Int63())
	cleanup := func() {
		background := context.Background()
		db.Exec(background, `DELETE FROM account_actions WHERE account_id=$1`, accountID)
		db.Exec(background, `DELETE FROM accounts WHERE id=$1`, accountID)
	}
	cleanup()
	t.Cleanup(cleanup)
	if _, err := db.Exec(ctx, `
		INSERT INTO accounts (id,provider,platform,"group",source_system,source_id,status,
			consecutive_failures,cooldown_until,excluded_models)
		VALUES ($1,'b51-prov','b51-plat','default','b51',$1,'active',
			4, now() + interval '5 minutes', '["b51-model"]'::jsonb)`, accountID); err != nil {
		t.Fatal(err)
	}
	return accountID
}

func b51Epoch(t *testing.T, ctx context.Context, db *pgxpool.Pool, accountID string) int64 {
	t.Helper()
	var epoch int64
	if err := db.QueryRow(ctx, `SELECT fence_epoch FROM accounts WHERE id=$1`, accountID).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	return epoch
}

// B5.1: disabling an account must take it out of the snapshot query, bump the
// epoch so an in-flight credential refresh loses its CAS, and leave an audit
// row. The epoch bump is the part worth asserting: without it a refresh already
// in flight would land on top of the operator's decision.
func TestConsoleDisableAccountFencesAndAudits(t *testing.T) {
	ctx, db, console := newConsoleTestConsole(t)
	accountID := b5Account(t, ctx, db, "disable")
	before := b51Epoch(t, ctx, db, accountID)

	response := b5Request(t, console, http.MethodPost, "/v1/accounts/"+accountID+"/status",
		`{"status":"disabled","reason":"suspected leak"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("disable: got %d (%s)", response.Code, response.Body.String())
	}
	var result accountActionResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Applied {
		t.Errorf("disable reported no change: %+v", result)
	}
	if result.FenceEpoch <= before {
		t.Errorf("fence_epoch %d did not advance past %d; an in-flight refresh could overwrite this", result.FenceEpoch, before)
	}
	// The response must not imply the change is already live on gateway.
	if result.EffectiveWithin <= 0 {
		t.Error("the response does not tell the operator the change arrives via the snapshot")
	}

	var status string
	if err := db.QueryRow(ctx, `SELECT status FROM accounts WHERE id=$1`, accountID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "disabled" {
		t.Errorf("status = %q, want disabled", status)
	}
	// The snapshot query selects status='active', so a disabled account is out
	// of rotation on the next publish. Assert that rather than trusting it.
	var schedulable int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE id=$1 AND status='active'`, accountID).Scan(&schedulable); err != nil {
		t.Fatal(err)
	}
	if schedulable != 0 {
		t.Error("a disabled account is still selectable by the snapshot query")
	}

	var action, reason, who string
	var auditEpoch int64
	if err := db.QueryRow(ctx, `SELECT action,reason,operator,fence_epoch FROM account_actions WHERE account_id=$1`,
		accountID).Scan(&action, &reason, &who, &auditEpoch); err != nil {
		t.Fatalf("no audit row was written: %v", err)
	}
	if action != "set_status:disabled" || reason != "suspected leak" || who != "b5-test-operator" {
		t.Errorf("audit row = %q/%q/%q", action, reason, who)
	}
	if auditEpoch != result.FenceEpoch {
		t.Errorf("audit epoch %d does not match the action's %d, so the row cannot be lined up with what it fenced", auditEpoch, result.FenceEpoch)
	}
}

// Re-disabling an already disabled account must not bump the epoch and must not
// write an audit row claiming a change that did not happen. A no-op that
// invalidated an in-flight refresh would make retries actively harmful.
func TestConsoleRepeatedDisableIsANoOpThatDoesNotFence(t *testing.T) {
	ctx, db, console := newConsoleTestConsole(t)
	accountID := b5Account(t, ctx, db, "noop")
	if response := b5Request(t, console, http.MethodPost, "/v1/accounts/"+accountID+"/status",
		`{"status":"disabled","reason":"first"}`); response.Code != http.StatusOK {
		t.Fatalf("first disable: %d (%s)", response.Code, response.Body.String())
	}
	afterFirst := b51Epoch(t, ctx, db, accountID)

	response := b5Request(t, console, http.MethodPost, "/v1/accounts/"+accountID+"/status",
		`{"status":"disabled","reason":"second"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("second disable: %d (%s)", response.Code, response.Body.String())
	}
	var result accountActionResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Applied {
		t.Error("a repeated disable reported itself as a change")
	}
	if result.Detail == "" {
		t.Error("a no-op did not say why nothing happened")
	}
	if got := b51Epoch(t, ctx, db, accountID); got != afterFirst {
		t.Errorf("a no-op moved fence_epoch from %d to %d, invalidating in-flight work for nothing", afterFirst, got)
	}
	var audits int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM account_actions WHERE account_id=$1`, accountID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Errorf("got %d audit rows, want 1: a no-op must not claim a change", audits)
	}
}

// A reason is mandatory for a status change. An outage nobody can explain later
// is the failure this guards.
func TestConsoleDisableRequiresAReason(t *testing.T) {
	ctx, db, console := newConsoleTestConsole(t)
	accountID := b5Account(t, ctx, db, "reason")
	response := b5Request(t, console, http.MethodPost, "/v1/accounts/"+accountID+"/status", `{"status":"disabled"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("a reasonless disable got %d, want 400 (%s)", response.Code, response.Body.String())
	}
	// And it must not have happened anyway.
	var status string
	if err := db.QueryRow(ctx, `SELECT status FROM accounts WHERE id=$1`, accountID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Errorf("the rejected request changed status to %q anyway", status)
	}
}

// Only the two states the rest of the system understands are accepted. An
// arbitrary string would make the account neither schedulable nor visibly
// broken.
func TestConsoleAccountStatusRejectsUnknownStates(t *testing.T) {
	ctx, db, console := newConsoleTestConsole(t)
	accountID := b5Account(t, ctx, db, "badstate")
	for _, bad := range []string{"quarantined", "", "ACTIVE; DROP TABLE accounts"} {
		body, _ := json.Marshal(map[string]string{"status": bad, "reason": "test"})
		response := b5Request(t, console, http.MethodPost, "/v1/accounts/"+accountID+"/status", string(body))
		if response.Code != http.StatusBadRequest {
			t.Errorf("status %q got %d, want 400", bad, response.Code)
		}
	}
	// The table survives, which it would not if the value reached SQL.
	if _, err := db.Exec(ctx, `SELECT 1 FROM accounts LIMIT 1`); err != nil {
		t.Fatalf("accounts is unusable after a hostile status: %v", err)
	}
}

// Clearing a cooldown must clear the failure streak with it -- otherwise the
// next failure re-escalates straight back into a cooldown, which is not what
// "clear it" means -- and must tell the operator that gateway's own local
// cooldown is not cleared by this.
func TestConsoleClearCooldownAlsoClearsTheStreakAndSaysWhatItCannotClear(t *testing.T) {
	ctx, db, console := newConsoleTestConsole(t)
	accountID := b5Account(t, ctx, db, "cooldown")

	response := b5Request(t, console, http.MethodPost, "/v1/accounts/"+accountID+"/cooldown/clear", `{"reason":"upstream recovered"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("clear cooldown: %d (%s)", response.Code, response.Body.String())
	}
	var result accountActionResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Applied {
		t.Error("clearing a live cooldown reported no change")
	}
	if result.GatewayLocalCooldownNote == "" {
		t.Error("the response does not warn that gateway's in-process cooldown is untouched; an operator would hunt a bug that is the design")
	}

	var cooldown *time.Time
	var failures int
	if err := db.QueryRow(ctx, `SELECT cooldown_until,consecutive_failures FROM accounts WHERE id=$1`,
		accountID).Scan(&cooldown, &failures); err != nil {
		t.Fatal(err)
	}
	if cooldown != nil {
		t.Errorf("cooldown_until = %v, want NULL", cooldown)
	}
	if failures != 0 {
		t.Errorf("consecutive_failures = %d, want 0 so the next failure does not re-escalate immediately", failures)
	}
}

func TestConsoleClearExcludedModelsEmptiesTheList(t *testing.T) {
	ctx, db, console := newConsoleTestConsole(t)
	accountID := b5Account(t, ctx, db, "excluded")

	response := b5Request(t, console, http.MethodPost, "/v1/accounts/"+accountID+"/excluded-models/clear", "")
	if response.Code != http.StatusOK {
		t.Fatalf("clear excluded models: %d (%s)", response.Code, response.Body.String())
	}
	var excluded []byte
	if err := db.QueryRow(ctx, `SELECT excluded_models FROM accounts WHERE id=$1`, accountID).Scan(&excluded); err != nil {
		t.Fatal(err)
	}
	if got := decodeStringArray(excluded); len(got) != 0 {
		t.Errorf("excluded_models = %v, want empty", got)
	}
	// An empty body must be accepted here: these actions take no required
	// input, so curl without -d should work.
	var audits int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM account_actions WHERE account_id=$1`, accountID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Errorf("got %d audit rows, want 1", audits)
	}
}

// A typo'd account id must be a 404, not a cheerful 200 saying it was already
// in the requested state.
func TestConsoleAccountActionOnUnknownAccountIsNotFound(t *testing.T) {
	_, _, console := newConsoleTestConsole(t)
	for _, path := range []string{"/status", "/cooldown/clear", "/excluded-models/clear"} {
		response := b5Request(t, console, http.MethodPost, "/v1/accounts/b51-does-not-exist"+path,
			`{"status":"disabled","reason":"test"}`)
		if response.Code != http.StatusNotFound {
			t.Errorf("%s on a missing account = %d, want 404 (%s)", path, response.Code, response.Body.String())
		}
	}
}

// The audit trail must be immutable in the database, the same way
// billing_resolutions and model_prices are. An audit trail that can be edited
// is not one.
func TestConsoleAccountActionsAreImmutable(t *testing.T) {
	ctx, db, console := newConsoleTestConsole(t)
	accountID := b5Account(t, ctx, db, "immutable")
	if response := b5Request(t, console, http.MethodPost, "/v1/accounts/"+accountID+"/status",
		`{"status":"disabled","reason":"original"}`); response.Code != http.StatusOK {
		t.Fatalf("disable: %s", response.Body.String())
	}
	if _, err := db.Exec(ctx, `UPDATE account_actions SET reason='rewritten' WHERE account_id=$1`, accountID); err == nil {
		t.Error("an account_actions row was updated; the audit trail is editable")
	}
	if _, err := db.Exec(ctx, `DELETE FROM account_actions WHERE account_id=$1`, accountID); err == nil {
		t.Error("an account_actions row was deleted; the audit trail is editable")
	}
}

// B5.1 pagination: a full page must be distinguishable from the end of the
// data, and following the cursor must return the rest without repeating or
// skipping a row. Skipping is the real risk -- a held row stepped over is
// revenue nobody ever rules on.
func TestConsoleHeldTrayPagesWithoutSkippingOrRepeating(t *testing.T) {
	ctx, db, console := newConsoleTestConsole(t)
	tenantID := fmt.Sprintf("b51-page-%d", rand.Int63())
	t.Cleanup(func() {
		background := context.Background()
		db.Exec(background, `DELETE FROM usage_ledger WHERE tenant_id=$1`, tenantID)
		db.Exec(background, `DELETE FROM tenants WHERE id=$1`, tenantID)
	})
	if _, err := db.Exec(ctx, `INSERT INTO tenants (id,name) VALUES ($1,'B5.1 paging')`, tenantID); err != nil {
		t.Fatal(err)
	}
	// Five rows sharing one occurred_at, so the id tie-breaker is exercised:
	// a cursor on the timestamp alone would loop or skip here.
	occurredAt := time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC)
	const total = 5
	for i := 0; i < total; i++ {
		insertTestUsage(ctx, t, db, tenantID, fmt.Sprintf("%s-event-%d", tenantID, i), "b51-model", "missing", false, occurredAt, [4]int64{0, 0, 0, 0})
	}
	if _, err := db.Exec(ctx, `UPDATE usage_ledger SET billing_state='held' WHERE tenant_id=$1`, tenantID); err != nil {
		t.Fatal(err)
	}

	seen := map[string]int{}
	cursor, pages := "", 0
	for {
		target := fmt.Sprintf("/v1/billing/held?tenant=%s&limit=2", tenantID)
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		response := b5Request(t, console, http.MethodGet, target, "")
		if response.Code != http.StatusOK {
			t.Fatalf("held page %d: %d (%s)", pages, response.Code, response.Body.String())
		}
		var body struct {
			Held       []heldRow `json:"held"`
			NextCursor string    `json:"next_cursor"`
			HasMore    bool      `json:"has_more"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, row := range body.Held {
			seen[row.EventID]++
		}
		pages++
		if pages > total+2 {
			t.Fatal("paging did not terminate; the cursor is not advancing")
		}
		if !body.HasMore {
			if body.NextCursor != "" {
				t.Error("has_more is false but a next_cursor was still handed out")
			}
			break
		}
		if body.NextCursor == "" {
			t.Fatal("has_more is true but no cursor was given, so the rest is unreachable")
		}
		cursor = body.NextCursor
	}
	if len(seen) != total {
		t.Errorf("paged over %d distinct rows, want %d: %v", len(seen), total, seen)
	}
	for eventID, count := range seen {
		if count != 1 {
			t.Errorf("row %s was returned %d times", eventID, count)
		}
	}
}

// The exact-multiple case: with 4 rows and a page size of 2, the second page is
// full but there is nothing after it. Inferring has_more from a full page would
// hand out a cursor to an empty third page, and "one last empty page" reads
// exactly like "the tray drained" to whoever is working it.
func TestConsoleHeldTrayDoesNotOfferACursorToAnEmptyPage(t *testing.T) {
	ctx, db, console := newConsoleTestConsole(t)
	tenantID := fmt.Sprintf("b51-exact-%d", rand.Int63())
	t.Cleanup(func() {
		background := context.Background()
		db.Exec(background, `DELETE FROM usage_ledger WHERE tenant_id=$1`, tenantID)
		db.Exec(background, `DELETE FROM tenants WHERE id=$1`, tenantID)
	})
	if _, err := db.Exec(ctx, `INSERT INTO tenants (id,name) VALUES ($1,'B5.1 exact multiple')`, tenantID); err != nil {
		t.Fatal(err)
	}
	occurredAt := time.Date(2021, 6, 2, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		insertTestUsage(ctx, t, db, tenantID, fmt.Sprintf("%s-event-%d", tenantID, i), "b51-model", "missing", false, occurredAt, [4]int64{0, 0, 0, 0})
	}
	if _, err := db.Exec(ctx, `UPDATE usage_ledger SET billing_state='held' WHERE tenant_id=$1`, tenantID); err != nil {
		t.Fatal(err)
	}

	cursor := ""
	for page := 0; page < 2; page++ {
		target := fmt.Sprintf("/v1/billing/held?tenant=%s&limit=2", tenantID)
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		response := b5Request(t, console, http.MethodGet, target, "")
		var body struct {
			Held       []heldRow `json:"held"`
			NextCursor string    `json:"next_cursor"`
			HasMore    bool      `json:"has_more"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Held) != 2 {
			t.Fatalf("page %d returned %d rows, want 2", page, len(body.Held))
		}
		if page == 1 {
			if body.HasMore || body.NextCursor != "" {
				t.Errorf("the last full page offered another: has_more=%v cursor=%q", body.HasMore, body.NextCursor)
			}
			return
		}
		cursor = body.NextCursor
	}
}

// A15: an adjustment is the only sanctioned way to correct money that has
// already been billed, so the properties that matter are that it moves the
// balance in both directions, that a retry cannot correct twice, and that the
// balance == sum(journal) identity B4.5 reconciles on still holds afterwards.
// The last one is the whole reason this endpoint exists instead of a psql
// UPDATE: a hand-written repair would register as wallet_balance_drift.
func TestConsoleAdjustCorrectsWalletAndStaysReconciled(t *testing.T) {
	ctx, db, console := newConsoleTestConsole(t)
	// b5Fixture rather than a bare tenant, and that choice is the whole point:
	// Reconciliation.Run builds its tenant set from usage_ledger rows inside
	// the window, so a tenant with no ledger rows is never examined and the
	// drift assertion below would pass without checking anything. The first
	// version of this test did exactly that -- it was green and vacuous. The
	// fixture's held row is enough to put the tenant in the window, and the
	// report.Tenants assertion further down keeps it honest.
	tenantID, _ := b5Fixture(t, ctx, db, "adjust")
	repository := PGBillingRepository{DB: db}

	// Negative: the under-charge case from B4 §D6. A top-up cannot express it.
	debit := `{"id":"` + tenantID + `-adj1","amount":"-2.5","reason":"late price entry for b5-model"}`
	if response := b5Request(t, console, http.MethodPost, "/v1/billing/wallets/"+tenantID+"/adjust", debit); response.Code != http.StatusCreated {
		t.Fatalf("negative adjustment: got %d (%s)", response.Code, response.Body.String())
	}
	// Replaying the same id must return the original rather than correct twice.
	replayed := b5Request(t, console, http.MethodPost, "/v1/billing/wallets/"+tenantID+"/adjust", debit)
	if replayed.Code != http.StatusOK {
		t.Fatalf("replayed adjustment: got %d (%s)", replayed.Code, replayed.Body.String())
	}
	var replay struct {
		Replayed bool `json:"replayed"`
	}
	if err := json.Unmarshal(replayed.Body.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed {
		t.Error("a repeated adjustment id was not reported as a replay")
	}

	// Positive: the over-charge refund direction.
	credit := `{"id":"` + tenantID + `-adj2","amount":"1.25","reason":"refund for incident 42"}`
	if response := b5Request(t, console, http.MethodPost, "/v1/billing/wallets/"+tenantID+"/adjust", credit); response.Code != http.StatusCreated {
		t.Fatalf("positive adjustment: got %d (%s)", response.Code, response.Body.String())
	}

	balance, exists, err := repository.Balance(ctx, tenantID)
	if err != nil || !exists {
		t.Fatalf("read balance: %v (exists=%v)", err, exists)
	}
	if balance != "8.750000000000" {
		t.Fatalf("balance %s after 10 - 2.5 (applied twice) + 1.25, want 8.750000000000", balance)
	}

	// Both rows are journalled as adjustments, with operator and reason. The
	// journal has no operator column by design, so the note is the audit trail.
	rows, err := repository.ListWalletTransactions(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	adjustments := 0
	for _, row := range rows {
		if row.Kind != "adjustment" {
			continue
		}
		adjustments++
		if !strings.Contains(row.Note, "b5-test-operator") {
			t.Errorf("adjustment %s note %q does not attribute the operator", row.ID, row.Note)
		}
	}
	if adjustments != 2 {
		t.Errorf("journal holds %d adjustment rows, want 2 (a replay must not add one)", adjustments)
	}

	// The identity B4.5 checks must survive the correction.
	report, err := (Reconciliation{DB: db}).Run(ctx, b5OccurredAt.Add(-time.Hour), time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// Non-vacuity guard, and it is load-bearing: Run only examines tenants
	// that have usage_ledger rows in the window, so without this the loop
	// below could iterate over a report that never looked at this wallet at
	// all. Assert the tenant was actually reconciled before trusting the
	// absence of a drift finding.
	examined := false
	for _, tenant := range report.Tenants {
		if tenant.TenantID == tenantID {
			examined = true
			if tenant.WalletBalance != "8.750000000000" || tenant.JournalSum != "8.750000000000" {
				t.Errorf("reconcile saw balance=%s journal=%s, want both 8.750000000000",
					tenant.WalletBalance, tenant.JournalSum)
			}
		}
	}
	if !examined {
		t.Fatal("reconcile never examined this tenant, so the drift assertion below proves nothing")
	}
	for _, discrepancy := range report.Discrepancies {
		if discrepancy.Kind == DiscrepancyWalletBalanceDrift && discrepancy.TenantID == tenantID {
			t.Fatalf("adjusting through the console drifted the wallet: %+v", discrepancy)
		}
	}
}

// Adjusting a tenant with no wallet is a 404, not a silently created wallet.
// A correction presumes something to correct; a mistyped tenant id must not
// look like success.
func TestConsoleAdjustRequiresAnExistingWallet(t *testing.T) {
	_, _, console := newConsoleTestConsole(t)
	response := b5Request(t, console, http.MethodPost,
		"/v1/billing/wallets/b5-no-such-tenant-adjust/adjust",
		`{"id":"b5-adjust-orphan","amount":"-1","reason":"should not apply"}`)
	if response.Code != http.StatusNotFound {
		t.Fatalf("adjust on a walletless tenant: got %d (%s), want 404", response.Code, response.Body.String())
	}
}
