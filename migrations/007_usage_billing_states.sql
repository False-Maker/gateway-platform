-- 007_usage_billing_states.sql
--
-- B4.3 widens usage_ledger.billing_state with `not_billable`.
--
-- `not_billable` and `held` are deliberately two states, not one:
--   held         -- a human has to decide before any money moves. The bucket
--                   that must stay small and must be alerted on.
--   not_billable -- decided: this row is worth zero and never will be worth
--                   anything. Terminal, and excluded from revenue.
-- Collapsing them would bury the handful of rows that represent real lost
-- revenue under every transient network error the platform has ever seen.
--
-- Note also that `unpriced` (our pricing configuration is incomplete) and
-- `missing` usage (the upstream did not tell us the token counts) stay
-- separate buckets, per B4-BILLING-MODEL's B4.3 addendum: one is our bug,
-- the other is theirs, and merging the counts hides both.
--
-- The constraint is dropped and re-added rather than guarded, because 006
-- already created a narrower version of it under the same name and every
-- migration file is replayed on each boot.

ALTER TABLE usage_ledger DROP CONSTRAINT IF EXISTS usage_ledger_billing_state_check;

ALTER TABLE usage_ledger ADD CONSTRAINT usage_ledger_billing_state_check
    CHECK (billing_state IN ('pending', 'billed', 'unpriced', 'held', 'not_billable'));

-- Held rows are queried by hand and by the alert path, always by why they
-- were held, so the index leads with usage_source.
CREATE INDEX IF NOT EXISTS usage_ledger_held_idx
    ON usage_ledger (usage_source, occurred_at) WHERE billing_state = 'held';

-- Each run reports its buckets separately. Summing held or not-billable rows
-- into rows_billed would make a run with zero revenue look healthy.
ALTER TABLE billing_runs ADD COLUMN IF NOT EXISTS rows_held INTEGER NOT NULL DEFAULT 0;
ALTER TABLE billing_runs ADD COLUMN IF NOT EXISTS rows_not_billable INTEGER NOT NULL DEFAULT 0;
