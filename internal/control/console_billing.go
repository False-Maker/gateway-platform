package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// B5: the console's billing surface.
//
// Three of these routes close gaps earlier tasks left open on purpose, and the
// comments below say which task and why the shape is what it is:
//   * held-row resolution -- B4.3's `held` state means "no machine may decide
//     this", and B4.3, B4.5 and the E1 alert rules each recorded that there was
//     no way for a human to decide it.
//   * top-up -- B4.1 left the funding entry point to B5.
//   * price creation -- B4.1 shipped the table and the immutability trigger but
//     no writer for it, so the pipeline had no way to be configured at all.

// maxConsoleBody bounds a request body. Every write here is a handful of
// fields; anything larger is a mistake or an attack, and either way there is
// no reason to read it.
const maxConsoleBody = 64 << 10

// decodeBody reads a JSON body with unknown fields rejected. Rejecting them
// matters more here than in most APIs: a typo in `amount` that silently
// defaults could move money, and the safer failure is a 400 that names the
// field.
func decodeBody(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxConsoleBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("request body is required")
		}
		return err
	}
	// Reject trailing content so a second document cannot be smuggled past the
	// first read.
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request body must contain exactly one JSON document")
	}
	return nil
}

// walletView is a tenant's wallet as the console reports it.
type walletView struct {
	TenantID  string             `json:"tenant_id"`
	Currency  string             `json:"currency"`
	Balance   contracts.Decimal  `json:"balance"`
	Blocked   bool               `json:"blocked"`
	RecoverAt string             `json:"recovers_now_at,omitempty"`
	Movement  []walletMovementVw `json:"movements"`
}

type walletMovementVw struct {
	ID           string            `json:"id"`
	Kind         string            `json:"kind"`
	Amount       contracts.Decimal `json:"amount"`
	BalanceAfter contracts.Decimal `json:"balance_after"`
	BillingRunID string            `json:"billing_run_id,omitempty"`
	Note         string            `json:"note,omitempty"`
	CreatedAt    string            `json:"created_at"`
}

// handleWallet reports a balance and its journal. `blocked` is computed the way
// B4.4's publish tick computes it -- balance <= 0 and a wallet row exists -- so
// the console and the gateway never disagree about whether a tenant is being
// refused. The comparison uses contracts.Decimal, never a float.
func (c Console) handleWallet(w http.ResponseWriter, r *http.Request) {
	if c.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "database is not configured")
		return
	}
	tenantID := strings.TrimSpace(r.PathValue("tenantID"))
	if tenantID == "" {
		writeError(w, http.StatusBadRequest, "tenant id is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	repository := PGBillingRepository{DB: c.DB}
	balance, exists, err := repository.Balance(ctx, tenantID)
	if err != nil {
		c.fail(w, "read wallet", err)
		return
	}
	if !exists {
		// No wallet is a real state and not a zero balance: B4.4 refuses only
		// tenants whose wallet exists and is empty, so a tenant with no wallet
		// row is never blocked.
		writeJSON(w, http.StatusOK, map[string]any{
			"wallet": nil, "blocked": false,
			"note": "tenant has no wallet; B4.4 never blocks a tenant without a wallet row",
		})
		return
	}
	transactions, err := repository.ListWalletTransactions(ctx, tenantID)
	if err != nil {
		c.fail(w, "list wallet transactions", err)
		return
	}
	view := walletView{TenantID: tenantID, Balance: balance, Blocked: walletBlocked(balance), Movement: []walletMovementVw{}}
	if err := c.DB.QueryRow(ctx, `SELECT currency FROM tenant_wallets WHERE tenant_id=$1`, tenantID).Scan(&view.Currency); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		c.fail(w, "read wallet currency", err)
		return
	}
	for _, transaction := range transactions {
		view.Movement = append(view.Movement, walletMovementVw{
			ID: transaction.ID, Kind: transaction.Kind, Amount: transaction.Amount,
			BalanceAfter: transaction.BalanceAfter, BillingRunID: transaction.BillingRunID,
			Note: transaction.Note, CreatedAt: transaction.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"wallet": view, "blocked": view.Blocked})
}

// walletBlocked is B4.4's rule, in one place so the console cannot drift from
// the publish tick that decides it. It is deliberately a function of the
// balance only: the "wallet exists" half of the rule is about the row, and the
// caller has already established that.
func walletBlocked(balance contracts.Decimal) bool {
	sign, ok := balance.Sign()
	return ok && sign <= 0
}

type topupRequest struct {
	// ID is the caller's idempotency key and becomes the wallet_transactions
	// primary key. Requiring it is the whole reason a retried top-up cannot
	// credit a tenant twice: the second attempt collides on the primary key
	// instead of adding money.
	ID     string            `json:"id"`
	Amount contracts.Decimal `json:"amount"`
	Note   string            `json:"note"`
}

// handleTopup credits a wallet. It is an operator action, not a payment
// integration: B4-BILLING-MODEL §2 puts payment channels out of scope, so this
// records money that arrived by some other means.
//
// Two details are deliberate:
//   - The wallet is created if it does not exist. A top-up is exactly the
//     moment a wallet comes into being, and requiring an "create wallet" step
//     first would let an operator credit nothing and see success.
//   - A repeated id is not an error. It returns the original movement, so a
//     retry after a timeout is safe and cannot double-credit.
func (c Console) handleTopup(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.PathValue("tenantID"))
	if tenantID == "" {
		writeError(w, http.StatusBadRequest, "tenant id is required")
		return
	}
	var request topupRequest
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(request.ID) == "" {
		writeError(w, http.StatusBadRequest, "id is required: it is the idempotency key for this top-up")
		return
	}
	if sign, ok := request.Amount.Sign(); !ok || sign <= 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("amount %q must be a positive decimal", request.Amount))
		return
	}
	// A20: see handleAdjust. handleTopup had the identical gap.
	if err := checkMoney("amount", request.Amount); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if c.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "database is not configured")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	repository := PGBillingRepository{DB: c.DB}
	if err := repository.EnsureWallet(ctx, tenantID, "USD"); err != nil {
		// tenant_wallets.tenant_id is a foreign key, so a wallet for a tenant
		// that does not exist fails here. Reported as 404 rather than 500:
		// "you named a tenant that does not exist" and "the console is broken"
		// must not look the same. No separate existence check is needed, which
		// also removes the window between such a check and this insert.
		if isForeignKeyViolation(err) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("no tenant %s", tenantID))
			return
		}
		c.fail(w, "ensure wallet", err)
		return
	}
	movement, err := repository.ApplyMovement(ctx, WalletMovement{
		ID: request.ID, TenantID: tenantID, Kind: "topup",
		Amount: request.Amount,
		Note:   topupNote(operator(r), request.Note),
	})
	if err != nil {
		// Same rule as handleAdjust. A top-up cannot reverse direction the way
		// an adjustment can -- the kind check keeps it positive -- but it could
		// still report someone else's row, or a different amount under this
		// tenant's own id, as "your retry succeeded".
		if isUniqueViolation(err) {
			replay, replayErr := c.confirmReplay(ctx, tenantID, request.ID, "topup", request.Amount)
			if replayErr != nil {
				c.metrics().AddCounter("control_console_topup_conflict_total", 1)
				writeError(w, http.StatusConflict, fmt.Sprintf(
					"top-up id %s cannot be reused: %s. Pick a new id; ids are global to wallet_transactions, not per tenant.",
					request.ID, replayErr))
				return
			}
			c.metrics().AddCounter("control_console_topup_replayed_total", 1)
			writeJSON(w, http.StatusOK, map[string]any{"movement": replay, "replayed": true})
			return
		}
		c.fail(w, "apply top-up", err)
		return
	}
	c.metrics().AddCounter("control_console_topup_total", 1)
	writeJSON(w, http.StatusCreated, map[string]any{
		"movement": walletMovementVw{
			ID: movement.ID, Kind: movement.Kind, Amount: movement.Amount,
			BalanceAfter: movement.BalanceAfter, BillingRunID: movement.BillingRunID,
			Note: movement.Note, CreatedAt: movement.CreatedAt.UTC().Format(time.RFC3339),
		},
		"replayed": false,
	})
}

type adjustRequest struct {
	// ID is the idempotency key, same contract as topupRequest.ID: a retried
	// correction must not correct twice.
	ID     string            `json:"id"`
	Amount contracts.Decimal `json:"amount"`
	// Reason is required. See handleAdjust.
	Reason string `json:"reason"`
}

// handleAdjust writes an explicit wallet correction.
//
// B4-BILLING-MODEL §D3 makes this the *only* sanctioned way to correct money
// that has already been billed: a priced ledger row is never recomputed when a
// price later changes, and B4.3's `unpriced` is terminal and can never be
// priced afterwards. Until this endpoint existed the platform had a validator
// for `adjustment` and a CHECK constraint permitting it, but no writer -- so
// an over-charge, a mis-charge, an upstream-incident refund and a closing
// tenant's remaining balance all had no entry point, and the only way out was
// a hand-written UPDATE. That bypasses applyMovement, which is the sole writer
// of tenant_wallets.balance, and therefore breaks the balance == sum(journal)
// identity B4.5 reconciles on: the repair would register as wallet_balance_drift.
//
// Three differences from handleTopup, all deliberate:
//   - The amount may be negative. Correcting an under-charge -- §D6's "a new
//     model shipped before its price did" -- has to move money *out*, which is
//     exactly what handleTopup refuses.
//   - The wallet must already exist. An adjustment presumes something to
//     correct; creating one here would turn a mistyped tenant id into a
//     silently successful no-op instead of a 404.
//   - reason is required. Months later a correction without a stated cause is
//     indistinguishable from a bug, and this row is what an audit reads.
func (c Console) handleAdjust(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.PathValue("tenantID"))
	if tenantID == "" {
		writeError(w, http.StatusBadRequest, "tenant id is required")
		return
	}
	var request adjustRequest
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(request.ID) == "" {
		writeError(w, http.StatusBadRequest, "id is required: it is the idempotency key for this adjustment")
		return
	}
	if strings.TrimSpace(request.Reason) == "" {
		writeError(w, http.StatusBadRequest, "reason is required: an adjustment must record what it corrects")
		return
	}
	// Zero is rejected here as well as in checkMovementKind so the caller gets
	// a 400 naming the field rather than a 500 from the repository.
	if sign, ok := request.Amount.Sign(); !ok || sign == 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("amount %q must be a non-zero decimal", request.Amount))
		return
	}
	// A20: the same rule the repository enforces, applied here so an amount the
	// money columns cannot hold comes back as a named 400 instead of a 500 from
	// PostgreSQL. Calling checkMoney rather than restating its limits keeps one
	// source of truth for what NUMERIC(38,12) accepts.
	if err := checkMoney("amount", request.Amount); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if c.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "database is not configured")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	repository := PGBillingRepository{DB: c.DB}
	movement, err := repository.ApplyMovement(ctx, WalletMovement{
		ID: request.ID, TenantID: tenantID, Kind: "adjustment",
		Amount: request.Amount,
		Note:   adjustmentNote(operator(r), request.Reason),
	})
	if err != nil {
		if errors.Is(err, ErrWalletNotFound) {
			writeError(w, http.StatusNotFound,
				fmt.Sprintf("no wallet for tenant %s: an adjustment corrects an existing wallet, top up to create one", tenantID))
			return
		}
		// Only a duplicate key may be read as a retry. Every other failure --
		// a rejected amount, a lock timeout, a broken connection -- is a
		// failure, and reporting it as a replay is how a correction gets lost.
		if isUniqueViolation(err) {
			replay, replayErr := c.confirmReplay(ctx, tenantID, request.ID, "adjustment", request.Amount)
			if replayErr != nil {
				c.metrics().AddCounter("control_console_adjustment_conflict_total", 1)
				writeError(w, http.StatusConflict, fmt.Sprintf(
					"adjustment id %s cannot be reused: %s. Pick a new id; ids are global to wallet_transactions, not per tenant.",
					request.ID, replayErr))
				return
			}
			c.metrics().AddCounter("control_console_adjustment_replayed_total", 1)
			writeJSON(w, http.StatusOK, map[string]any{"movement": replay, "replayed": true})
			return
		}
		c.fail(w, "apply adjustment", err)
		return
	}
	c.metrics().AddCounter("control_console_adjustment_total", 1)
	writeJSON(w, http.StatusCreated, map[string]any{
		"movement": walletMovementVw{
			ID: movement.ID, Kind: movement.Kind, Amount: movement.Amount,
			BalanceAfter: movement.BalanceAfter, BillingRunID: movement.BillingRunID,
			Note: movement.Note, CreatedAt: movement.CreatedAt.UTC().Format(time.RFC3339),
		},
		"replayed": false,
	})
}

// A replay is only a replay when the journal already holds *this* movement.
// These two say why it was not.
var (
	// The id collided on a primary key that spans every tenant, but the row
	// carrying it is not this tenant's.
	errReplayForeignTenant = errors.New("wallet transaction id is already used by another tenant")
	// The id is this tenant's, but it names a different movement than the one
	// being asked for.
	errReplayMismatch = errors.New("wallet transaction id was already used for a different movement")
)

// confirmReplay decides what a duplicate-key failure on a wallet write means.
//
// The rule it enforces: a retry may only be reported as a successful replay if
// the journal row it finds is the movement the caller actually asked for. The
// first version of this code returned *any* row with a matching (id, tenant)
// and treated *any* repository error as a replay, which had a money-losing
// failure mode -- reproduced against PostgreSQL before this was written:
// a top-up had taken the id "ticket-4711"; a later `adjust` of -500 reusing
// that id collided on the primary key, found the +500 top-up, and returned
// 200 {"replayed": true, "kind": "topup"}. The debit never happened and the
// caller saw success. Direction reversed, silently.
//
// wallet_transactions.id is global, not per-tenant, so kind and amount both
// have to be checked, and a collision with another tenant's row has to be
// distinguishable from "this is your retry".
func (c Console) confirmReplay(ctx context.Context, tenantID, id, kind string, amount contracts.Decimal) (walletMovementVw, error) {
	replay, ok := c.replayMovement(ctx, tenantID, id)
	if !ok {
		// The caller is here because of a primary-key violation, so a row with
		// this id exists somewhere; not finding it under this tenant means it
		// belongs to another one. Reporting that beats the 500 this used to be.
		return walletMovementVw{}, errReplayForeignTenant
	}
	if replay.Kind != kind || !sameMoney(replay.Amount, amount) {
		return walletMovementVw{}, fmt.Errorf("%w: it holds a %s of %s", errReplayMismatch, replay.Kind, replay.Amount)
	}
	return replay, nil
}

// replayMovement returns an existing movement when a write failed because its
// id was already used. It reads the row back rather than assuming, so the
// response describes what is actually in the journal.
func (c Console) replayMovement(ctx context.Context, tenantID, id string) (walletMovementVw, bool) {
	var movement walletMovementVw
	var createdAt time.Time
	err := c.DB.QueryRow(ctx, `
		SELECT id,kind,amount::text,balance_after::text,COALESCE(billing_run_id,''),note,created_at
		FROM wallet_transactions WHERE id=$1 AND tenant_id=$2`, id, tenantID).
		Scan(&movement.ID, &movement.Kind, &movement.Amount, &movement.BalanceAfter,
			&movement.BillingRunID, &movement.Note, &createdAt)
	if err != nil {
		return walletMovementVw{}, false
	}
	movement.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	return movement, true
}

// topupNote stamps the operator into the journal's note. The journal has no
// operator column and B5 is not going to add one to a table B4.5 reconciles;
// the note is where the human attribution belongs anyway, since it travels with
// the money.
func topupNote(who, note string) string {
	note = strings.TrimSpace(note)
	if note == "" {
		return "console top-up by " + who
	}
	return fmt.Sprintf("console top-up by %s: %s", who, note)
}

// adjustmentNote stamps operator and reason into the journal note, for the same
// reason topupNote does. Unlike a top-up the reason is mandatory, so there is
// no bare-attribution form.
func adjustmentNote(who, reason string) string {
	return fmt.Sprintf("console adjustment by %s: %s", who, strings.TrimSpace(reason))
}

// priceView is a price row as the console reports it.
type priceView struct {
	ID              string            `json:"id"`
	Provider        string            `json:"provider"`
	Model           string            `json:"model"`
	Currency        string            `json:"currency"`
	UnitScale       int64             `json:"unit_scale"`
	PriceInput      contracts.Decimal `json:"price_input"`
	PriceOutput     contracts.Decimal `json:"price_output"`
	PriceCacheRead  contracts.Decimal `json:"price_cache_read"`
	PriceCacheWrite contracts.Decimal `json:"price_cache_write"`
	EffectiveFrom   string            `json:"effective_from"`
	CreatedAt       string            `json:"created_at"`
}

// handlePrices lists every price row. There is no window parameter: the table
// is immutable and small by construction (a change is a new row), so the whole
// thing is the useful view -- an operator needs to see the history to know what
// a past bill was replayed against.
func (c Console) handlePrices(w http.ResponseWriter, r *http.Request) {
	if c.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "database is not configured")
		return
	}
	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rows, err := c.DB.Query(ctx, `
		SELECT id,provider,model,currency,unit_scale,
		       price_input::text,price_output::text,price_cache_read::text,price_cache_write::text,
		       effective_from,created_at
		FROM model_prices
		WHERE ($1::text = '' OR provider = $1::text)
		  AND ($2::text = '' OR model = $2::text)
		ORDER BY provider, model, effective_from DESC`, provider, model)
	if err != nil {
		c.fail(w, "query model prices", err)
		return
	}
	defer rows.Close()
	prices := []priceView{}
	for rows.Next() {
		var price priceView
		var effectiveFrom, createdAt time.Time
		if err := rows.Scan(&price.ID, &price.Provider, &price.Model, &price.Currency, &price.UnitScale,
			&price.PriceInput, &price.PriceOutput, &price.PriceCacheRead, &price.PriceCacheWrite,
			&effectiveFrom, &createdAt); err != nil {
			c.fail(w, "scan model price", err)
			return
		}
		price.EffectiveFrom = effectiveFrom.UTC().Format(time.RFC3339)
		price.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		prices = append(prices, price)
	}
	if err := rows.Err(); err != nil {
		c.fail(w, "read model prices", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"prices": prices, "count": len(prices)})
}

type insertPriceRequest struct {
	ID              string            `json:"id"`
	Provider        string            `json:"provider"`
	Model           string            `json:"model"`
	Currency        string            `json:"currency"`
	UnitScale       int64             `json:"unit_scale"`
	PriceInput      contracts.Decimal `json:"price_input"`
	PriceOutput     contracts.Decimal `json:"price_output"`
	PriceCacheRead  contracts.Decimal `json:"price_cache_read"`
	PriceCacheWrite contracts.Decimal `json:"price_cache_write"`
	// EffectiveFrom is required and must be in the future. D3 forbids a price
	// that takes effect retroactively, so this is not a warning the repository
	// would emit anyway -- it is the operator stating the effective instant on
	// purpose rather than inheriting "now".
	EffectiveFrom string `json:"effective_from"`
}

// handleInsertPrice appends a price. It cannot change an existing one: the
// repository offers no update and the database trigger refuses one even from
// psql, which is what makes a past bill replayable.
//
// It is also not a billing action. Nothing recalculates already-settled rows;
// a late price only ever applies to usage that occurred after it took effect,
// and usage that occurred before it becomes `unpriced`, which B4.3 decided is
// terminal and is corrected by an explicit wallet adjustment rather than by
// backdating.
func (c Console) handleInsertPrice(w http.ResponseWriter, r *http.Request) {
	var request insertPriceRequest
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(request.ID) == "" || strings.TrimSpace(request.Provider) == "" || strings.TrimSpace(request.Model) == "" {
		writeError(w, http.StatusBadRequest, "id, provider and model are required")
		return
	}
	effectiveFrom, err := parseConsoleTime(request.EffectiveFrom)
	if err != nil {
		writeError(w, http.StatusBadRequest, "effective_from: "+err.Error())
		return
	}
	if effectiveFrom.IsZero() {
		writeError(w, http.StatusBadRequest, "effective_from is required and must be in the future")
		return
	}
	now := c.now()
	if !effectiveFrom.After(now) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("effective_from %s is not after now (%s); D3 has prices apply forward only",
			effectiveFrom.Format(time.RFC3339), now.Format(time.RFC3339)))
		return
	}
	if c.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "database is not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	repository := PGBillingRepository{DB: c.DB}
	if err := repository.InsertModelPrice(ctx, ModelPrice{
		ID: request.ID, Provider: request.Provider, Model: request.Model,
		Currency: request.Currency, UnitScale: request.UnitScale,
		PriceInput: request.PriceInput, PriceOutput: request.PriceOutput,
		PriceCacheRead: request.PriceCacheRead, PriceCacheWrite: request.PriceCacheWrite,
		EffectiveFrom: effectiveFrom,
	}, now); err != nil {
		switch {
		case errors.Is(err, ErrPriceNotEffectiveInFuture), errors.Is(err, ErrPriceRejected):
			writeError(w, http.StatusBadRequest, err.Error())
		case isUniqueViolation(err):
			writeError(w, http.StatusConflict, "a price with this id, or the same (provider, model, effective_from), already exists")
		default:
			// A29: only the two cases above are the caller's fault. Everything
			// else -- an unreachable database, a lock timeout, the 10s context
			// expiring -- is this platform failing, and it used to come back as
			// a 400 quoting the driver verbatim. That told an operator to go fix
			// their input during an outage, kept the failure out of every 5xx
			// signal, and put the database user, database name and address in a
			// response body, which Console.fail exists to prevent.
			c.fail(w, "insert model price", err)
		}
		return
	}
	c.metrics().AddCounter("control_console_price_total", 1, "provider", request.Provider)
	writeJSON(w, http.StatusCreated, map[string]any{"id": request.ID, "effective_from": effectiveFrom.Format(time.RFC3339)})
}

// handleReconcile runs B4.5's three-way reconciliation over a window. It is
// read-only by construction -- Reconciliation.Run opens a RepeatableRead
// read-only transaction -- so a console request can never repair a difference,
// only report one. Both window bounds are required: a default window would
// hide the boundary choice, and the window is over usage_ledger.occurred_at,
// which B4.5 explains at length.
func (c Console) handleReconcile(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	start, err := parseConsoleTime(query.Get("start"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "start: "+err.Error())
		return
	}
	end, err := parseConsoleTime(query.Get("end"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "end: "+err.Error())
		return
	}
	if start.IsZero() || end.IsZero() {
		writeError(w, http.StatusBadRequest, "start and end are required (RFC3339); the window is over usage_ledger.occurred_at")
		return
	}
	if c.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "database is not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	report, err := (Reconciliation{DB: c.DB}).Run(ctx, start, end)
	if err != nil {
		c.fail(w, "run reconciliation", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"report": report, "balanced": report.Balanced()})
}

// isUniqueViolation reports whether an error is a PostgreSQL unique-key
// violation, so the console can turn "this id is already used" into a 409
// instead of a 400 that hides what went wrong.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// isForeignKeyViolation reports whether an error is a PostgreSQL foreign-key
// violation. The only foreign key the console's writes can trip is tenant_id,
// so this is how "you named a tenant that does not exist" becomes a 404 rather
// than a 500 that reads like a broken console.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23503"
	}
	return false
}
