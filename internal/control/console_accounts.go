package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// B5.1: the three manual account actions §6.5 asks for. B5 left the account
// board read-only and recorded that as a gap.
//
// All three change what gateway will select, and none of them reaches gateway
// directly -- §3's P0 decision is that control and gateway share only the
// snapshot and the Release stream, with no synchronous RPC. So every action
// here writes PostgreSQL and lets the A1 snapshot loop carry the change on its
// next tick (snapshotRefreshInterval, one minute). The response says so
// explicitly rather than letting an operator believe the effect was immediate:
// "I disabled it and it still served three requests" is the kind of surprise
// that gets a design blamed for a delay it documented.
//
// Each action increments fence_epoch. That is not decoration: a credential
// refresh or wrapper job already in flight holds an epoch, and an operator
// action that did not move the epoch would let that in-flight writer land its
// result on top of the operator's decision. Bumping the epoch makes the stale
// writer fail its CAS, which is exactly what ErrFenceLost exists for.
//
// Each action also appends to account_actions in the same transaction as the
// account update. An operator action that changed scheduling but left no record
// of who did it is the thing this path cannot afford -- the same argument
// billing_resolutions makes for money.

// accountActionResult is the shared response shape. Epoch and EffectiveWithin
// are the two things an operator actually needs afterwards.
type accountActionResult struct {
	AccountID string `json:"account_id"`
	Action    string `json:"action"`
	Applied   bool   `json:"applied"`
	// Detail explains a no-op, e.g. the account was already disabled.
	Detail     string `json:"detail,omitempty"`
	FenceEpoch int64  `json:"fence_epoch"`
	// EffectiveWithin names the snapshot delay in seconds. Gateway reads a
	// snapshot, so nothing here takes effect the moment it returns 200.
	EffectiveWithin int `json:"effective_within_seconds"`
	// GatewayLocalCooldownNote is present on the cooldown action only, and
	// exists because clearing the database cooldown does not clear gateway's.
	GatewayLocalCooldownNote string `json:"gateway_local_cooldown_note,omitempty"`
}

var errAccountNotFound = errors.New("account not found")

// handleAccountStatus disables or re-enables an account.
//
// Only 'active' and 'disabled' are accepted. The status column also carries
// values the authority layer sets on its own, and letting an operator type an
// arbitrary string here would let them invent a state the snapshot query and
// A9's release handler have never heard of -- the account would then be neither
// schedulable nor visibly broken.
func (c Console) handleAccountStatus(w http.ResponseWriter, r *http.Request) {
	if c.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "database is not configured")
		return
	}
	accountID := strings.TrimSpace(r.PathValue("accountID"))
	var body struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON: "+err.Error())
		return
	}
	status := strings.TrimSpace(strings.ToLower(body.Status))
	if status != "active" && status != "disabled" {
		writeError(w, http.StatusBadRequest, `status must be "active" or "disabled"`)
		return
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		// A disable with no reason is an outage nobody can explain later. It
		// costs the operator one field and saves the next person an hour.
		writeError(w, http.StatusBadRequest, "reason is required so the action can be explained later")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := c.applyAccountAction(ctx, accountID, "set_status:"+status, reason, operator(r),
		`UPDATE accounts SET status=$2, fence_epoch=fence_epoch+1, updated_at=now()
		 WHERE id=$1 AND status <> $2
		 RETURNING fence_epoch`)
	if err != nil {
		c.accountActionError(w, "set account status", err)
		return
	}
	if !result.Applied {
		result.Detail = "account was already " + status
	}
	writeJSON(w, http.StatusOK, result)
}

// handleClearCooldown ends a control-side cooldown early.
//
// The note in the response is the important part. Gateway keeps its own
// in-process cooldown table (the bridge described in §6.3.1), and control
// cannot clear it: there is no RPC, and the snapshot does not carry it. So this
// clears the authoritative cooldown and tells the operator plainly that a
// gateway instance may still be sitting out the remainder of its local one.
// Silently implying otherwise would send someone hunting a bug that is the
// documented design.
func (c Console) handleClearCooldown(w http.ResponseWriter, r *http.Request) {
	if c.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "database is not configured")
		return
	}
	accountID := strings.TrimSpace(r.PathValue("accountID"))
	reason := consoleReason(r)

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	// consecutive_failures is reset with the cooldown on purpose: leaving it
	// would let the very next failure re-escalate straight back into a
	// cooldown, which is not what "clear it" means to the person asking.
	result, err := c.applyAccountAction(ctx, accountID, "clear_cooldown", reason, operator(r),
		`UPDATE accounts SET cooldown_until=NULL, consecutive_failures=0,
		        fence_epoch=fence_epoch+1, updated_at=now()
		 WHERE id=$1 AND (cooldown_until IS NOT NULL OR consecutive_failures > 0)
		 RETURNING fence_epoch`)
	if err != nil {
		c.accountActionError(w, "clear account cooldown", err)
		return
	}
	if !result.Applied {
		result.Detail = "account had no cooldown and no failure streak"
	}
	result.GatewayLocalCooldownNote = "gateway keeps a separate in-process cooldown that control cannot clear; " +
		"an instance may still skip this account until its own local timer expires"
	writeJSON(w, http.StatusOK, result)
}

// handleClearExcludedModels empties the per-model exclusion list an account
// accumulated from forbidden_capability responses.
//
// This is the lowest-risk of the three: if the exclusions were right, they
// simply accumulate again the next time the upstream refuses those models. It
// is all-or-nothing rather than per-model because a partial list is a state an
// operator would have to reason about, and the recovery cost of being wrong is
// one more refusal per model.
func (c Console) handleClearExcludedModels(w http.ResponseWriter, r *http.Request) {
	if c.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "database is not configured")
		return
	}
	accountID := strings.TrimSpace(r.PathValue("accountID"))
	reason := consoleReason(r)

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := c.applyAccountAction(ctx, accountID, "clear_excluded_models", reason, operator(r),
		`UPDATE accounts SET excluded_models='[]'::jsonb, fence_epoch=fence_epoch+1, updated_at=now()
		 WHERE id=$1 AND excluded_models <> '[]'::jsonb
		 RETURNING fence_epoch`)
	if err != nil {
		c.accountActionError(w, "clear excluded models", err)
		return
	}
	if !result.Applied {
		result.Detail = "account had no excluded models"
	}
	writeJSON(w, http.StatusOK, result)
}

// applyAccountAction runs one account update and its audit row in a single
// transaction.
//
// The update statements all carry a "nothing to do" guard in their WHERE
// clause, so a no-op returns no rows. That distinction is kept rather than
// collapsed: re-disabling an already disabled account must not bump the epoch
// (which would invalidate an in-flight refresh for no reason) and must not
// write an audit row claiming a change that did not happen. The audit row is
// only written when something actually moved.
func (c Console) applyAccountAction(ctx context.Context, accountID, action, reason, who, updateSQL string) (accountActionResult, error) {
	result := accountActionResult{
		AccountID:       accountID,
		Action:          action,
		EffectiveWithin: int(snapshotRefreshInterval / time.Second),
	}
	if accountID == "" {
		return result, fmt.Errorf("%w: empty id", errAccountNotFound)
	}
	tx, err := c.DB.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)

	// Confirm the account exists before deciding whether a no-op was "already
	// in that state" or "no such account". Without this, disabling a typo'd id
	// would return a cheerful 200 saying it was already disabled.
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT true FROM accounts WHERE id=$1`, accountID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return result, errAccountNotFound
		}
		return result, err
	}

	var epoch int64
	var args []any
	switch {
	case strings.HasPrefix(action, "set_status:"):
		args = []any{accountID, strings.TrimPrefix(action, "set_status:")}
	default:
		args = []any{accountID}
	}
	err = tx.QueryRow(ctx, updateSQL, args...).Scan(&epoch)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Nothing changed. Read the current epoch so the response is still
		// truthful about the account's state.
		if err := tx.QueryRow(ctx, `SELECT fence_epoch FROM accounts WHERE id=$1`, accountID).Scan(&epoch); err != nil {
			return result, err
		}
		result.FenceEpoch = epoch
		return result, tx.Commit(ctx)
	case err != nil:
		return result, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO account_actions (account_id, action, reason, operator, fence_epoch)
		 VALUES ($1,$2,$3,$4,$5)`, accountID, action, reason, who, epoch); err != nil {
		return result, err
	}
	result.Applied, result.FenceEpoch = true, epoch
	return result, tx.Commit(ctx)
}

func (c Console) accountActionError(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, errAccountNotFound) {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	c.fail(w, what, err)
}

// consoleReason reads an optional reason from the body of an action that does
// not require one. Unlike disabling an account, clearing a cooldown or an
// exclusion list is reversible by simply waiting, so a reason is recorded when
// offered but not demanded.
func consoleReason(r *http.Request) string {
	var body struct {
		Reason string `json:"reason"`
	}
	// A missing or unparsable body is not an error here: these two actions
	// take no required input, so an operator using curl without -d should not
	// be made to send `{}`.
	_ = json.NewDecoder(http.MaxBytesReader(nil, r.Body, 16<<10)).Decode(&body)
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		return "not given"
	}
	if len(reason) > 500 {
		reason = reason[:500]
	}
	return reason
}
