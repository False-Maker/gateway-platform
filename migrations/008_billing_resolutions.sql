-- B5 console: the audit trail for held rows a human has ruled on.
--
-- B4.3 deliberately left `held` rows without an entry point: the state means
-- "no machine may decide this", so the decision has to come from outside the
-- billing job. This table is where that decision is recorded, and it is the
-- reason the console's resolve endpoint is not simply an UPDATE.
--
-- Why a separate table rather than columns on usage_ledger:
--   * A ledger row is a fact about a request and stays minimal; the decision
--     about it carries an operator, a note and a timestamp of its own.
--   * `billed` rows produced by the B4.2 job have no resolution, so a nullable
--     column group would be NULL on every row the job writes.
--   * The tray is append-only, so "who wrote this off, and why" survives even
--     though usage_ledger's own state moves on.
--
-- The decision is recorded even when it moves no money (write_off, or a bill
-- that turned out unpriced), because the operator's ruling is the artifact
-- that matters: without it the row would simply look untouched.
--
-- `billing_run_id` holds the synthetic run id the console writes to
-- usage_ledger and to wallet_transactions, so a manually settled row is
-- indistinguishable from a job-settled one to B4.5's reconciliation: it finds
-- one run id, one ledger total and one matching wallet debit.
CREATE TABLE IF NOT EXISTS billing_resolutions (
    event_id TEXT PRIMARY KEY REFERENCES usage_ledger(event_id) ON DELETE CASCADE,
    tenant_id TEXT NOT NULL,
    -- bill: the row is chargeable at the price in effect at occurred_at.
    -- write_off: the row is decided to be worth zero (billing_state
    --            not_billable). Not the same as leaving it held.
    decision TEXT NOT NULL,
    -- outcome records what actually happened, which is not always what was
    -- asked for: `bill` on a row with no price in effect becomes unpriced.
    outcome TEXT NOT NULL,
    -- Amount debited, NULL when the decision moved no money. A '0' here would
    -- be indistinguishable from a settled row that genuinely cost nothing.
    amount NUMERIC(38,12),
    billing_run_id TEXT,
    operator TEXT NOT NULL DEFAULT '',
    note TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT billing_resolutions_decision_check CHECK (decision IN ('bill', 'write_off')),
    CONSTRAINT billing_resolutions_outcome_check CHECK (outcome IN ('billed', 'unpriced', 'not_billable')),
    CONSTRAINT billing_resolutions_amount_check CHECK (
        (outcome = 'billed' AND amount IS NOT NULL AND amount >= 0)
        OR (outcome <> 'billed' AND amount IS NULL)
    ),
    CONSTRAINT billing_resolutions_run_check CHECK (
        (outcome = 'not_billable' AND billing_run_id IS NULL)
        OR (outcome <> 'not_billable' AND billing_run_id IS NOT NULL)
    )
);

-- The held tray is browsed oldest-first, and by tenant.
CREATE INDEX IF NOT EXISTS billing_resolutions_created_idx
    ON billing_resolutions (created_at DESC);
CREATE INDEX IF NOT EXISTS billing_resolutions_tenant_idx
    ON billing_resolutions (tenant_id, created_at DESC);
