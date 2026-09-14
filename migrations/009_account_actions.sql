-- B5.1: the audit trail for manual account actions.
--
-- The three console actions (disable/enable, clear cooldown, clear the model
-- exclusion list) all change what gateway will schedule. billing_resolutions
-- makes the same argument for money that this table makes for scheduling: an
-- operator action that changed behaviour but left no record of who made it and
-- why cannot be explained afterwards, and "why did this account come back into
-- rotation at 03:00" is exactly the question someone will ask.
--
-- Append-only, like billing_resolutions and model_prices. The row is written in
-- the same transaction as the accounts update, so an action either leaves both
-- the effect and its explanation or neither.
CREATE TABLE IF NOT EXISTS account_actions (
    id BIGSERIAL PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    action TEXT NOT NULL,
    reason TEXT NOT NULL,
    -- operator is what the caller claimed via X-Operator. It is attribution
    -- for a human reading this table, NOT an authenticated identity: the
    -- console has capability levels, not an operator directory. Anyone who
    -- treats this column as proof of who acted is reading more into it than
    -- the auth model puts there.
    operator TEXT NOT NULL,
    -- The epoch this action produced, so a row here can be lined up against
    -- the credential refresh or wrapper job it fenced out.
    fence_epoch BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT account_actions_action_check CHECK (
        action IN ('set_status:active', 'set_status:disabled', 'clear_cooldown', 'clear_excluded_models')
    )
);

CREATE INDEX IF NOT EXISTS account_actions_account_idx
    ON account_actions (account_id, created_at DESC);
CREATE INDEX IF NOT EXISTS account_actions_created_idx
    ON account_actions (created_at DESC);

-- Same immutability rule as billing_resolutions: an audit trail that can be
-- edited is not one. Enforced in the database so a manual psql session cannot
-- rewrite it either.
CREATE OR REPLACE FUNCTION account_actions_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'account_actions rows are immutable: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS account_actions_immutable_trigger ON account_actions;
CREATE TRIGGER account_actions_immutable_trigger
    BEFORE UPDATE OR DELETE ON account_actions
    FOR EACH ROW EXECUTE FUNCTION account_actions_immutable();
