-- B4.1 billing schema: model prices and prepaid tenant wallets.
-- Money is exact decimal everywhere: NUMERIC in PG, contracts.Decimal in Go.
-- Never float64 (0.01 is not representable, and B4.5 requires reconciliation
-- differences to be explainable down to a single event_id).
--
-- Gateway never reads these tables. Prices are read only by control's slow
-- path (the B4.2 deduction job); wallet balance reaches gateway through the
-- existing A6 token snapshot, never by a query.
CREATE TABLE IF NOT EXISTS model_prices (
    id TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    currency TEXT NOT NULL DEFAULT 'USD',
    -- Number of tokens one unit price covers (e.g. 1000000 = price per 1M tokens).
    unit_scale BIGINT NOT NULL,
    price_input NUMERIC(38,12) NOT NULL,
    price_output NUMERIC(38,12) NOT NULL,
    price_cache_read NUMERIC(38,12) NOT NULL,
    price_cache_write NUMERIC(38,12) NOT NULL,
    effective_from TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT model_prices_unit_scale_check CHECK (unit_scale > 0),
    CONSTRAINT model_prices_nonnegative_check CHECK (
        price_input >= 0 AND price_output >= 0
        AND price_cache_read >= 0 AND price_cache_write >= 0
    ),
    CONSTRAINT model_prices_effective_key UNIQUE (provider, model, effective_from)
);

-- Price lookup is always "the row in effect at a ledger row's occurred_at",
-- so the index is descending on effective_from.
CREATE INDEX IF NOT EXISTS model_prices_lookup_idx
    ON model_prices (provider, model, effective_from DESC);

-- D3: a price change only ever applies forward. An existing row may never be
-- updated or deleted, so a bill can always be replayed against the price that
-- was in effect when the usage happened. Enforced in the database as well as
-- in the application so a manual psql session cannot rewrite history either.
CREATE OR REPLACE FUNCTION model_prices_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'model_prices rows are immutable: insert a new row with a later effective_from instead of % on %', TG_OP, OLD.id;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS model_prices_immutable_trigger ON model_prices;
CREATE TRIGGER model_prices_immutable_trigger
    BEFORE UPDATE OR DELETE ON model_prices
    FOR EACH ROW EXECUTE FUNCTION model_prices_immutable();

-- D4: prepaid wallet. balance is the current amount; wallet_transactions is
-- the immutable journal. The two must always move in the same transaction --
-- B4.5 reconciles the journal sum against this balance.
CREATE TABLE IF NOT EXISTS tenant_wallets (
    tenant_id TEXT PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
    currency TEXT NOT NULL DEFAULT 'USD',
    balance NUMERIC(38,12) NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS wallet_transactions (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenant_wallets(tenant_id) ON DELETE CASCADE,
    -- topup and adjustment are operator actions; debit is produced by the
    -- B4.2 billing run identified by billing_run_id.
    kind TEXT NOT NULL,
    -- Signed: topup is positive, debit is negative, adjustment is either.
    amount NUMERIC(38,12) NOT NULL,
    balance_after NUMERIC(38,12) NOT NULL,
    billing_run_id TEXT,
    note TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wallet_transactions_kind_check CHECK (kind IN ('topup', 'debit', 'adjustment')),
    CONSTRAINT wallet_transactions_amount_check CHECK (
        (kind = 'topup' AND amount > 0)
        OR (kind = 'debit' AND amount < 0)
        OR (kind = 'adjustment' AND amount <> 0)
    )
);

CREATE INDEX IF NOT EXISTS wallet_transactions_tenant_idx
    ON wallet_transactions (tenant_id, created_at DESC);

CREATE INDEX IF NOT EXISTS wallet_transactions_run_idx
    ON wallet_transactions (billing_run_id) WHERE billing_run_id IS NOT NULL;
