-- B4.2 billing state on the usage ledger, plus the billing run journal.
-- Billing is a slow-path job in control: gateway writes usage_ledger rows and
-- never reads or updates any of these columns.
--
-- billing_state:
--   pending  -- not yet examined, or deliberately left for B4.3 to rule on
--   billed   -- priced and debited (billed_amount may be 0 for a zero-token row)
--   unpriced -- no price was in effect at occurred_at; terminal, because D3
--               forbids backdated prices, so this row can never become
--               priceable. Correcting it needs an explicit wallet adjustment.
--   held     -- reserved for B4.3 (usage we do not trust enough to bill)
ALTER TABLE usage_ledger ADD COLUMN IF NOT EXISTS billing_state TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE usage_ledger ADD COLUMN IF NOT EXISTS billed_amount NUMERIC(38,12);
ALTER TABLE usage_ledger ADD COLUMN IF NOT EXISTS billing_run_id TEXT;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'usage_ledger_billing_state_check') THEN
        ALTER TABLE usage_ledger ADD CONSTRAINT usage_ledger_billing_state_check
            CHECK (billing_state IN ('pending', 'billed', 'unpriced', 'held'));
    END IF;
END
$$;

-- The job only ever scans rows that are not settled yet.
CREATE INDEX IF NOT EXISTS usage_ledger_billing_idx
    ON usage_ledger (tenant_id, occurred_at) WHERE billing_state = 'pending';

CREATE TABLE IF NOT EXISTS billing_runs (
    id TEXT PRIMARY KEY,
    -- Rows with occurred_at < window_end are eligible; anything later waits
    -- for the next run, so a late-arriving row is never lost, only deferred.
    window_end TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL DEFAULT 'running',
    tenants_billed INTEGER NOT NULL DEFAULT 0,
    tenants_failed INTEGER NOT NULL DEFAULT 0,
    rows_billed INTEGER NOT NULL DEFAULT 0,
    rows_unpriced INTEGER NOT NULL DEFAULT 0,
    total_debited NUMERIC(38,12) NOT NULL DEFAULT 0,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    CONSTRAINT billing_runs_status_check CHECK (status IN ('running', 'completed', 'failed', 'skipped'))
);
