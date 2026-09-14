package control

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

// B5: resolving a held row, which is the entry point B4.3, B4.5 and the E1
// alert rules each recorded as missing.
//
// A `held` row exists because B4.3 refused to let a machine decide: either the
// request succeeded and the upstream never said how many tokens it used, or the
// usage is of a kind no one has ruled on yet. The alert on the held gauge says
// "a human has to look at this". This file is where the human's ruling is
// recorded and applied, and the shape of the flow follows from those origins:
//
//   - The decision is one of two, and both are explicit. `bill` charges the row
//     at the price in effect at its occurred_at, exactly as the B4.2 job would
//     have. `write_off` records that the row is worth zero, which is a decision,
//     not an absence of one -- it moves the row to `not_billable`, the state
//     B4.3 created specifically so a write-off cannot be confused with a row
//     nobody has looked at.
//   - A `bill` decision can still come back `unpriced`. D3 forbids backdated
//     prices, so a held row with no price in effect has no price it could ever
//     be charged at; the honest outcome is the terminal `unpriced` state, not a
//     refused request. The response says which of the two happened.
//   - The ruling is written to billing_resolutions in the same transaction as
//     the ledger update. An operator decision that moved money but left no
//     record of who made it would be the one thing this whole path cannot
//     afford.
//   - A manually billed row carries a synthetic billing_run_id instead of a
//     real run's. B4.5's reconciliation groups by that column and compares each
//     group's ledger total against the single wallet debit carrying the same
//     id, so a manual bill that follows the same convention reconciles exactly
//     like a job-settled one. Nothing in the reconciliation needs to know a
//     human was involved, and that is the point: the money has to add up the
//     same way either way.

// heldRow is a held ledger row as the tray reports it. It carries the fields a
// human needs to make the call -- what was used, how it ended, and why the
// machine would not decide -- and nothing else.
type heldRow struct {
	EventID     string `json:"event_id"`
	TenantID    string `json:"tenant_id"`
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	AccountID   string `json:"account_id"`
	StatusCode  int    `json:"status_code"`
	ErrorClass  string `json:"error_class"`
	UsageSource string `json:"usage_source"`
	Partial     bool   `json:"partial"`
	// Token counts are reported even though B4.3 did not trust them enough to
	// bill automatically: they are the evidence the operator is ruling on.
	TokensIn         int64  `json:"tokens_in"`
	TokensOut        int64  `json:"tokens_out"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	OccurredAt       string `json:"occurred_at"`
	BillingRunID     string `json:"billing_run_id,omitempty"`
	// HeldBecause explains why the billing job parked it. It is derived from
	// the same policy table the job uses, so the tray cannot describe a row
	// differently from the reason it was actually held.
	HeldBecause string `json:"held_because"`
}

type heldResolution struct {
	EventID      string `json:"event_id"`
	TenantID     string `json:"tenant_id"`
	Decision     string `json:"decision"`
	Outcome      string `json:"outcome"`
	Amount       string `json:"amount,omitempty"`
	BillingRunID string `json:"billing_run_id,omitempty"`
	Operator     string `json:"operator"`
	Note         string `json:"note,omitempty"`
	CreatedAt    string `json:"created_at"`
}

// handleHeld lists the held tray, oldest first: the oldest row is the one that
// has been blocking revenue visibility longest, and the alert fires on the
// count, so the order matches the pressure.
func (c Console) handleHeld(w http.ResponseWriter, r *http.Request) {
	if c.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "database is not configured")
		return
	}
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenant"))
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
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// The keyset predicate mirrors the ORDER BY exactly: (occurred_at, id)
	// strictly after the last row of the previous page. Oldest first, because
	// the oldest held row is the one that has been waiting on a human longest
	// -- which is also why offset paging would be wrong here, since it is
	// precisely those rows that a shifting offset drops.
	rows, err := c.DB.Query(ctx, `
		SELECT event_id,tenant_id,provider,model,account_id,status_code,error_class,
		       usage_source,partial,tokens_in,tokens_out,cache_read_tokens,cache_write_tokens,
		       occurred_at,COALESCE(billing_run_id,''),id
		FROM usage_ledger
		WHERE billing_state='held' AND ($1::text = '' OR tenant_id = $1::text)
		  AND ($3::text = '' OR (occurred_at, id) > ($3::timestamptz, $4::bigint))
		ORDER BY occurred_at, id
		LIMIT $2`, tenantID, fetchLimit(limit), cursor.Sort, cursor.ID)
	if err != nil {
		c.fail(w, "query held rows", err)
		return
	}
	defer rows.Close()
	held := []heldRow{}
	// lastSort/lastID track the final row actually returned, which is what the
	// next cursor must name -- not the extra probe row, which the caller has
	// not seen.
	var lastSort string
	var lastID int64
	// hasMore is set only by actually seeing the probe row. Inferring it from
	// len(held) == limit instead would hand out a cursor to an empty page
	// whenever the backlog happens to be an exact multiple of the page size,
	// and "one last empty page" is indistinguishable from "the tray drained"
	// to whoever is working it.
	hasMore := false
	for rows.Next() {
		var row heldRow
		var occurredAt time.Time
		var id int64
		if err := rows.Scan(&row.EventID, &row.TenantID, &row.Provider, &row.Model, &row.AccountID,
			&row.StatusCode, &row.ErrorClass, &row.UsageSource, &row.Partial,
			&row.TokensIn, &row.TokensOut, &row.CacheReadTokens, &row.CacheWriteTokens,
			&occurredAt, &row.BillingRunID, &id); err != nil {
			c.fail(w, "scan held row", err)
			return
		}
		if len(held) == limit {
			// The probe row proves there is more; it is not part of this page.
			hasMore = true
			break
		}
		row.OccurredAt = occurredAt.UTC().Format(time.RFC3339)
		row.HeldBecause = heldReason(row.UsageSource, row.Partial, row.StatusCode, row.ErrorClass)
		held = append(held, row)
		lastSort, lastID = occurredAt.UTC().Format(time.RFC3339Nano), id
	}
	if err := rows.Err(); err != nil {
		c.fail(w, "read held rows", err)
		return
	}
	next := ""
	if hasMore {
		next = encodeCursor(lastSort, lastID)
	}
	writeJSON(w, http.StatusOK, pageResponse("held", held, len(held), next))
}

// heldReason restates the policy table's verdict for one row. An unknown class
// says so explicitly rather than being reported as a normal hold, because the
// two mean different things: one is a gap in the policy table and the other is
// a decision B4.3 made.
func heldReason(usageSource string, partial bool, statusCode int, errorClass string) string {
	class := usageClass{source: usageSource, partial: partial, succeeded: releaseSucceeded(statusCode, errorClass)}
	if _, known := usageDispositions[class]; !known {
		return "no policy entry for this usage class; the policy table is behind the contract"
	}
	switch class.source {
	case contracts.UsageSourceEstimated:
		return "usage is estimated, not measured; nothing in this platform produces it yet"
	case contracts.UsageSourceMissing:
		return "the request was served but the upstream never reported token counts"
	default:
		return "held by policy"
	}
}

type resolveHeldRequest struct {
	// Decision is bill or write_off. There is no default: a missing or
	// unrecognised decision is rejected rather than treated as either, because
	// both outcomes move money or write it off.
	Decision string `json:"decision"`
	Note     string `json:"note"`
}

// handleResolveHeld applies a human's ruling to one held row.
//
// The row is locked FOR UPDATE and re-checked inside the transaction, so a
// double-clicked resolve, or a resolve racing the billing job, cannot apply the
// same decision twice: the second caller sees a row that is no longer held and
// gets a 409.
func (c Console) handleResolveHeld(w http.ResponseWriter, r *http.Request) {
	eventID := strings.TrimSpace(r.PathValue("eventID"))
	if eventID == "" {
		writeError(w, http.StatusBadRequest, "event id is required")
		return
	}
	var request resolveHeldRequest
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	decision := strings.TrimSpace(request.Decision)
	if decision != "bill" && decision != "write_off" {
		writeError(w, http.StatusBadRequest, `decision must be "bill" or "write_off"`)
		return
	}
	if c.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "database is not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	resolution, err := c.resolveHeld(ctx, eventID, decision, operator(r), request.Note)
	switch {
	case errors.Is(err, errNotHeld):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "no ledger row with that event id")
		return
	case err != nil:
		c.fail(w, "resolve held row", err)
		return
	}
	c.metrics().AddCounter("control_console_held_resolved_total", 1, "decision", decision, "outcome", resolution.Outcome)
	if resolution.Outcome == "unpriced" {
		// A bill that could not be priced is a hold that did not become
		// revenue. It is counted separately so a tray that is emptying does
		// not look like a backlog that is being collected.
		c.metrics().AddCounter("control_console_held_unpriced_total", 1)
	}
	writeJSON(w, http.StatusOK, map[string]any{"resolution": resolution, "outcome": resolution.Outcome})
}

// resolveHeld is the whole decision in one transaction: read and lock the row,
// price it if asked to bill it, move the money, update the ledger, and append
// the ruling. Either all of that lands or none of it does.
func (c Console) resolveHeld(ctx context.Context, eventID, decision, who, note string) (heldResolution, error) {
	tx, err := c.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return heldResolution{}, fmt.Errorf("begin held resolution: %w", err)
	}
	defer tx.Rollback(ctx)

	var (
		tenantID         string
		provider         string
		model            string
		usageSource      string
		partial          bool
		statusCode       int
		errorClass       string
		tokensIn         int64
		tokensOut        int64
		cacheReadTokens  int64
		cacheWriteTokens int64
		occurredAt       time.Time
		state            string
	)
	err = tx.QueryRow(ctx, `
		SELECT tenant_id,provider,model,usage_source,partial,status_code,error_class,
		       tokens_in,tokens_out,cache_read_tokens,cache_write_tokens,occurred_at,billing_state
		FROM usage_ledger WHERE event_id=$1
		FOR UPDATE`, eventID).
		Scan(&tenantID, &provider, &model, &usageSource, &partial, &statusCode, &errorClass,
			&tokensIn, &tokensOut, &cacheReadTokens, &cacheWriteTokens, &occurredAt, &state)
	if err != nil {
		return heldResolution{}, err
	}
	if state != "held" {
		return heldResolution{}, fmt.Errorf("%w: event %s is %s", errNotHeld, eventID, state)
	}

	resolution := heldResolution{
		EventID: eventID, TenantID: tenantID, Decision: decision,
		Operator: who, Note: strings.TrimSpace(note),
	}
	if resolution.Operator == "" {
		resolution.Operator = "unknown"
	}

	if decision == "write_off" {
		resolution.Outcome = "not_billable"
		if _, err := tx.Exec(ctx, `
			UPDATE usage_ledger SET billing_state='not_billable', billed_amount=NULL, billing_run_id=NULL
			WHERE event_id=$1`, eventID); err != nil {
			return heldResolution{}, fmt.Errorf("write off event %s: %w", eventID, err)
		}
	} else {
		// A manual bill is a one-row billing run. The run id it writes is the
		// same string on the ledger row and on the wallet debit, which is
		// exactly what B4.5 groups by.
		runID, err := newHeldResolutionRunID()
		if err != nil {
			return heldResolution{}, err
		}
		price, found, err := priceAt(ctx, tx, provider, model, occurredAt)
		if err != nil {
			return heldResolution{}, err
		}
		if !found {
			// D3: no backdated price, so this row can never be priced. The
			// outcome mirrors what the billing job would have produced, run id
			// included -- the job stamps unpriced rows with the run that
			// examined them, and an operator's examination is no different.
			resolution.Outcome = "unpriced"
			resolution.BillingRunID = runID
			if _, err := tx.Exec(ctx, `
				UPDATE usage_ledger SET billing_state='unpriced', billed_amount=NULL, billing_run_id=$2
				WHERE event_id=$1`, eventID, runID); err != nil {
				return heldResolution{}, fmt.Errorf("mark event %s unpriced: %w", eventID, err)
			}
		} else {
			amount, err := lineAmountFromCounts(tokensIn, tokensOut, cacheReadTokens, cacheWriteTokens, price)
			if err != nil {
				return heldResolution{}, fmt.Errorf("price event %s: %w", eventID, err)
			}
			rounded := amount.FloatString(moneyScale)
			resolution.Outcome = "billed"
			resolution.Amount = rounded
			resolution.BillingRunID = runID
			// A zero amount settles the row without a journal entry, matching
			// the job: a zero-amount transaction would be noise in B4.5.
			if sign, _ := contracts.Decimal(rounded).Sign(); sign > 0 {
				if _, err := applyMovement(ctx, tx, WalletMovement{
					ID: runID + ":" + tenantID, TenantID: tenantID, Kind: "debit",
					Amount:       contracts.Decimal("-" + rounded),
					BillingRunID: runID,
					Note:         fmt.Sprintf("held row %s billed by %s", eventID, resolution.Operator),
				}); err != nil {
					return heldResolution{}, err
				}
			}
			if _, err := tx.Exec(ctx, `
				UPDATE usage_ledger SET billing_state='billed', billed_amount=$2::numeric, billing_run_id=$3
				WHERE event_id=$1`, eventID, rounded, runID); err != nil {
				return heldResolution{}, fmt.Errorf("mark event %s billed: %w", eventID, err)
			}
		}
	}

	var storedAmount any
	if resolution.Outcome == "billed" {
		storedAmount = resolution.Amount
	}
	var storedRun any
	if resolution.BillingRunID != "" {
		storedRun = resolution.BillingRunID
	}
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `
		INSERT INTO billing_resolutions (event_id,tenant_id,decision,outcome,amount,billing_run_id,operator,note)
		VALUES ($1,$2,$3,$4,$5::numeric,$6,$7,$8)
		RETURNING created_at`,
		eventID, tenantID, decision, resolution.Outcome, storedAmount, storedRun,
		resolution.Operator, resolution.Note).Scan(&createdAt); err != nil {
		return heldResolution{}, fmt.Errorf("record resolution for %s: %w", eventID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return heldResolution{}, fmt.Errorf("commit held resolution for %s: %w", eventID, err)
	}
	resolution.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	return resolution, nil
}

// lineAmountFromCounts is lineAmount for a row that is not a billableRow. It
// exists rather than being inlined so the two pricing paths cannot drift: both
// go through the same big.Rat arithmetic and the same unit_scale division.
func lineAmountFromCounts(tokensIn, tokensOut, cacheRead, cacheWrite int64, price ModelPrice) (*big.Rat, error) {
	return lineAmount(billableRow{
		tokensIn: tokensIn, tokensOut: tokensOut,
		cacheReadTokens: cacheRead, cacheWriteTokens: cacheWrite,
	}, price)
}

func newHeldResolutionRunID() (string, error) {
	return newBillingRunIDPrefixed("held")
}
