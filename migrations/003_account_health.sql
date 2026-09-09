-- A9 control-side ErrorClass authority. These columns are written only by
-- control when it consumes a terminal Release (internal/control/health.go)
-- and read by the snapshot loop; gateway never sees cooldown_until directly,
-- it simply stops receiving the account in the next snapshot.
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS consecutive_failures INTEGER NOT NULL DEFAULT 0;
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS cooldown_until TIMESTAMPTZ;
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS excluded_models JSONB NOT NULL DEFAULT '[]'::jsonb;

CREATE INDEX IF NOT EXISTS accounts_cooldown_idx ON accounts (platform, cooldown_until) WHERE cooldown_until IS NOT NULL;
