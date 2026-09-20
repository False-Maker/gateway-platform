package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/elucid/gateway-platform/internal/detail"
	"github.com/elucid/gateway-platform/internal/observability"
	"github.com/jackc/pgx/v5/pgxpool"
)

// B5: the console's backend API. 总览 §6.5 reserves the console itself for P6
// and names the three hooks P0 must leave behind -- account health, billing,
// request detail. This is those three hooks as an HTTP surface, plus the two
// write paths earlier tasks deliberately left without an entry point (B4.3's
// held rows and B4.1's top-up) and a price entry point without which the whole
// billing pipeline has no way to be configured.
//
// What this deliberately is not:
//   * Not a UI. Nothing here serves HTML; a browser front end is a separate
//     concern and would bring its own dependency chain into a repository that
//     has kept one.
//   * Not a second control loop. Every handler is either a read or a single
//     operator action; nothing here runs on a timer or holds control state.
//   * Not a metering dependency. A console request that fails changes nothing
//     about billing -- the job and the ledger are the authority, and the
//     console only reads them or appends a decision they will honour.
//
// Every endpoint is read-only except three: resolving a held row, topping up a
// wallet, and inserting a price. Each of those writes exactly one thing, in one
// transaction, and everything it writes is append-only, so a console action can
// always be explained afterwards by the row it left behind.

// Console is the backend API's dependencies. A nil field disables the routes
// that need it, which is how a deployment without ClickHouse still gets an
// account board instead of a 500 on every request that touches detail.
type Console struct {
	Auth    ConsoleAuth
	DB      *pgxpool.Pool
	Metrics *observability.Registry
	// Detail is the A12 request-detail store. Nil (or a writer with no URL)
	// means detail is off, which §6.4 treats as a supported deployment.
	Detail detail.ClickHouseWriter
	// Now is injectable for tests.
	Now func() time.Time
	// Limiter is injectable for tests. Nil builds the default token bucket.
	Limiter *consoleLimiter
}

func (c Console) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c Console) metrics() *observability.Registry {
	if c.Metrics != nil {
		return c.Metrics
	}
	return observability.Default
}

// PrimeMetrics publishes the console counters whose alerts cannot survive a
// missing first increment. A22: the registry creates a series on first
// AddCounter and the exposition has no _created, so increase() cannot see the
// increment that brought the series into existence. `held_unpriced` is a
// once-in-a-while event -- a single manual resolution landing on `unpriced`,
// which is revenue that was served and can never be collected -- so that one
// increment is the whole signal ConsoleHeldUnpricedResolution watches.
//
// The other console counters (topup, adjustment, conflict, replayed, rejected)
// are deliberately left alone: they are routine, recurring operator traffic, so
// only the very first event of a process is missed. Recorded in TODO A22.
func (c Console) PrimeMetrics() {
	c.metrics().AddCounter("control_console_held_unpriced_total", 0)
}

// Handler returns the console's routes.
//
// The mux is built here rather than on the global DefaultServeMux so that the
// console's routes can never be reached through another server that happens to
// share the process -- the metrics server in particular.
func (c Console) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", c.handleHealth)
	mux.HandleFunc("GET /v1/accounts", c.handleAccounts)
	mux.HandleFunc("POST /v1/accounts/{accountID}/status", c.handleAccountStatus)
	mux.HandleFunc("POST /v1/accounts/{accountID}/cooldown/clear", c.handleClearCooldown)
	mux.HandleFunc("POST /v1/accounts/{accountID}/excluded-models/clear", c.handleClearExcludedModels)

	mux.HandleFunc("GET /v1/billing/wallets/{tenantID}", c.handleWallet)
	mux.HandleFunc("POST /v1/billing/wallets/{tenantID}/topup", c.handleTopup)
	mux.HandleFunc("POST /v1/billing/wallets/{tenantID}/adjust", c.handleAdjust)
	mux.HandleFunc("GET /v1/billing/prices", c.handlePrices)
	mux.HandleFunc("POST /v1/billing/prices", c.handleInsertPrice)
	mux.HandleFunc("GET /v1/billing/held", c.handleHeld)
	mux.HandleFunc("POST /v1/billing/held/{eventID}/resolve", c.handleResolveHeld)
	mux.HandleFunc("GET /v1/billing/reconcile", c.handleReconcile)

	mux.HandleFunc("GET /v1/requests", c.handleRequests)

	return c.authorize(c.rateLimit(mux))
}

// authorize wraps the mux: nothing inside is reachable without an operator
// token, including routes added later, because the check is on the way in and
// not repeated per handler.
//
// Write capability is decided here too, by method rather than by path. Every
// mutating route in this package is a POST and every read is a GET, so "POST
// requires the read-write token" needs no list that a later handler could be
// forgotten from -- a new endpoint is read-only until it is a POST, and the
// moment it is a POST it is already protected.
func (c Console) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The health probe is authenticated too. It reveals nothing an
		// unauthenticated caller needs, and an unauthenticated route is one
		// more thing to reason about; keep the surface uniform instead.
		access := c.Auth.access(r)
		if access == consoleDenied {
			if !c.Auth.Enabled() {
				log.Printf("console request rejected: %s is not set, the console is disabled", ConsoleTokenEnv)
			}
			c.metrics().AddCounter("control_console_rejected_total", 1, "path", r.URL.Path)
			writeError(w, http.StatusUnauthorized, "operator token required")
			return
		}
		if r.Method != http.MethodGet && access != consoleReadWrite {
			c.metrics().AddCounter("control_console_rejected_total", 1, "path", r.URL.Path)
			log.Printf("console write to %s refused: caller holds the read-only token (claimed operator %q)", r.URL.Path, operator(r))
			writeError(w, http.StatusForbidden, "this token may read but not write")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (c Console) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := map[string]any{
		"database": c.DB != nil,
		"detail":   strings.TrimSpace(c.Detail.URL) != "",
		"now":      c.now(),
	}
	writeJSON(w, http.StatusOK, status)
}

// accountView is an account row as the console reports it. The omission is the
// point: no credential, no base_url, no proxy, no model_mapping. §6.4.1 keeps
// those out of anything a client can reach, and the console is reachable by
// more than one human.
type accountView struct {
	ID                  string   `json:"id"`
	Provider            string   `json:"provider"`
	Platform            string   `json:"platform"`
	Group               string   `json:"group"`
	Status              string   `json:"status"`
	LastErrorClass      string   `json:"last_error_class,omitempty"`
	LastErrorAt         string   `json:"last_error_at,omitempty"`
	ConsecutiveFailures int      `json:"consecutive_failures"`
	CooldownUntil       string   `json:"cooldown_until,omitempty"`
	ExcludedModels      []string `json:"excluded_models"`
	// Capabilities is the model list this account claims to serve. A31: an
	// empty list is not "no models" -- chooser.go's supports() reads it as
	// "matches every model", which is how A18's unreadable channel became the
	// most permissive account in the pool. An operator deciding whether to
	// re-enable a parked account has to be able to see that, and the board was
	// the only place they were going to look.
	Capabilities []string `json:"capabilities"`
	UpdatedAt    string   `json:"updated_at"`
}

// handleAccounts is §6.5's account health board. The columns are the ones A9
// writes and the snapshot loop reads; nothing here is computed on the fly,
// because "why is this account out of rotation" must be answerable from state
// control itself recorded, not from a fresh opinion.
func (c Console) handleAccounts(w http.ResponseWriter, r *http.Request) {
	if c.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "database is not configured")
		return
	}
	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	limit, err := consoleLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cursor, err := parseCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, cursorError(err))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	// Sorted and paged by id alone. accounts.id is unique, so it needs no
	// tie-breaker, and the previous (provider, platform, id) ordering cannot
	// be a keyset key without carrying all three through the cursor for no
	// benefit -- the board is grouped by the caller, not by the query.
	rows, err := c.DB.Query(ctx, `
		SELECT id,provider,platform,"group",status,
		       COALESCE(last_error_class,''),
		       last_error_at, consecutive_failures, cooldown_until,
		       excluded_models, capabilities, updated_at
		FROM accounts
		WHERE ($1::text = '' OR provider = $1::text)
		  AND ($2::text = '' OR status = $2::text)
		  AND ($4::text = '' OR id > $4::text)
		ORDER BY id
		LIMIT $3`, provider, status, fetchLimit(limit), cursor.Sort)
	if err != nil {
		c.fail(w, "query accounts", err)
		return
	}
	defer rows.Close()
	accounts := []accountView{}
	hasMore := false
	for rows.Next() {
		var account accountView
		var lastErrorAt, cooldownUntil *time.Time
		var updatedAt time.Time
		var excluded, capabilities []byte
		if err := rows.Scan(&account.ID, &account.Provider, &account.Platform, &account.Group, &account.Status,
			&account.LastErrorClass, &lastErrorAt, &account.ConsecutiveFailures, &cooldownUntil,
			&excluded, &capabilities, &updatedAt); err != nil {
			c.fail(w, "scan account", err)
			return
		}
		if len(accounts) == limit {
			hasMore = true
			break
		}
		// Timestamps are rendered as strings so callers do not have to guess
		// a layout, matching the detail store's own UTC formatting.
		account.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
		if lastErrorAt != nil {
			account.LastErrorAt = lastErrorAt.UTC().Format(time.RFC3339)
		}
		if cooldownUntil != nil {
			account.CooldownUntil = cooldownUntil.UTC().Format(time.RFC3339)
		}
		account.ExcludedModels = decodeStringArray(excluded)
		account.Capabilities = decodeStringArray(capabilities)
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		c.fail(w, "read accounts", err)
		return
	}
	next := ""
	if hasMore {
		next = encodeCursor(accounts[len(accounts)-1].ID, 0)
	}
	writeJSON(w, http.StatusOK, pageResponse("accounts", accounts, len(accounts), next))
}

// handleRequests is §6.5's live request log, read from A12's detail store. It
// is the one endpoint that talks to ClickHouse rather than PostgreSQL, and it
// reports the store being absent as 503 with a reason, not as an empty list --
// "no detail configured" and "no requests matched" must not look alike.
//
// Nothing here sums or counts detail as if it were the ledger. A12 says so
// explicitly, so the response carries rows only.
func (c Console) handleRequests(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit, err := detail.ParseLimit(query.Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	filter := detail.QueryFilter{TenantID: strings.TrimSpace(query.Get("tenant")), Provider: strings.TrimSpace(query.Get("provider")), Limit: limit}
	if filter.Start, err = parseConsoleTime(query.Get("start")); err != nil {
		writeError(w, http.StatusBadRequest, "start: "+err.Error())
		return
	}
	if filter.End, err = parseConsoleTime(query.Get("end")); err != nil {
		writeError(w, http.StatusBadRequest, "end: "+err.Error())
		return
	}
	if strings.TrimSpace(c.Detail.URL) == "" {
		writeError(w, http.StatusServiceUnavailable, detail.ErrStoreDisabled.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	records, err := c.Detail.Query(ctx, filter)
	if err != nil {
		c.fail(w, "query request detail", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": records, "count": len(records)})
}

// consoleLimit parses a `limit` query parameter. A bad value is an error rather
// than a silent default, for the same reason detail.ParseLimit rejects one: a
// caller that asked for 5000 rows and got 500 must not be left believing it saw
// everything.
func consoleLimit(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultConsoleLimit, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 || limit > maxConsoleLimit {
		return 0, fmt.Errorf("limit %q must be a positive integer no greater than %d", raw, maxConsoleLimit)
	}
	return limit, nil
}

const (
	defaultConsoleLimit = 500
	maxConsoleLimit     = 5000
)

// parseConsoleTime accepts RFC3339, which is what every other interface in
// this repository emits. An empty string means "no bound".
func parseConsoleTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not an RFC3339 timestamp", raw)
	}
	return parsed.UTC(), nil
}

// decodeStringArray reads the JSONB array columns (excluded_models) into a Go
// slice. A malformed value yields an empty slice rather than an error: this is
// display-only data and one bad row must not blank the whole board.
func decodeStringArray(raw []byte) []string {
	if len(raw) == 0 {
		return []string{}
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return []string{}
	}
	return values
}

func (c Console) fail(w http.ResponseWriter, what string, err error) {
	// The error goes to the log, not to the caller: these are operator-facing
	// endpoints, but a database error still describes internals the response
	// has no reason to carry.
	log.Printf("console %s failed: %v", what, err)
	writeError(w, http.StatusInternalServerError, what+" failed")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// errNotHeld is returned when a resolution names a row that is not currently
// held. It is a conflict rather than a 404: the row exists, its state is just
// not the one the operator thought.
var errNotHeld = errors.New("ledger row is not held")
