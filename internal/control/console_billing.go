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
		if replay, ok := c.replayTopup(ctx, tenantID, request.ID); ok {
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

// replayTopup returns an existing movement when a top-up failed because its id
// was already used. It reads the row back rather than assuming, so the response
// describes what is actually in the journal.
func (c Console) replayTopup(ctx context.Context, tenantID, id string) (walletMovementVw, bool) {
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
		case errors.Is(err, ErrPriceNotEffectiveInFuture):
			writeError(w, http.StatusBadRequest, err.Error())
		case isUniqueViolation(err):
			writeError(w, http.StatusConflict, "a price with this id, or the same (provider, model, effective_from), already exists")
		default:
			writeError(w, http.StatusBadRequest, err.Error())
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
