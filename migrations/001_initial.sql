-- Gateway-platform P0 schema. This migration intentionally uses text IDs so it
-- does not require a PostgreSQL extension in a dry-run environment.
CREATE TABLE IF NOT EXISTS accounts (
    id TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    platform TEXT NOT NULL,
    "group" TEXT NOT NULL DEFAULT 'default',
    source_system TEXT NOT NULL,
    source_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    fence_epoch BIGINT NOT NULL DEFAULT 0,
    credential_id TEXT,
    base_url TEXT NOT NULL DEFAULT '',
    proxy TEXT NOT NULL DEFAULT '',
    model_mapping JSONB NOT NULL DEFAULT '{}'::jsonb,
    capabilities JSONB NOT NULL DEFAULT '[]'::jsonb,
    limits JSONB NOT NULL DEFAULT '{"degrade_policy":"fail_closed"}'::jsonb,
    profile JSONB NOT NULL DEFAULT '{}'::jsonb,
    quota JSONB NOT NULL DEFAULT '{"items":[]}'::jsonb,
    last_error_class TEXT,
    last_error_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT accounts_source_key UNIQUE (source_system, source_id)
);

CREATE INDEX IF NOT EXISTS accounts_provider_group_status_idx
    ON accounts (provider, "group", status);

-- P3 quota snapshot publication outbox. The quota write and this enqueue are
-- committed together; Redis publication is retried asynchronously.
CREATE TABLE IF NOT EXISTS quota_snapshot_outbox (
    account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    platform TEXT NOT NULL,
    "group" TEXT NOT NULL,
    fence_epoch BIGINT NOT NULL,
    quota JSONB NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_until TIMESTAMPTZ,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS quota_snapshot_outbox_due_idx
    ON quota_snapshot_outbox (next_attempt_at, updated_at);

CREATE TABLE IF NOT EXISTS credentials (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    encrypted_secret BYTEA NOT NULL,
    expires_at TIMESTAMPTZ,
    version BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT credentials_account_key UNIQUE (account_id)
);

CREATE TABLE IF NOT EXISTS request_attempts (
    attempt_id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    deadline_at TIMESTAMPTZ NOT NULL,
    terminal_event_id TEXT UNIQUE,
    state TEXT NOT NULL DEFAULT 'open',
    reconciled_at TIMESTAMPTZ,
    account_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    CONSTRAINT request_attempts_state_check CHECK (state IN ('open', 'terminal', 'reconciled'))
);

CREATE INDEX IF NOT EXISTS request_attempts_state_deadline_idx
    ON request_attempts (state, deadline_at);
CREATE INDEX IF NOT EXISTS request_attempts_request_idx
    ON request_attempts (request_id);

CREATE TABLE IF NOT EXISTS usage_ledger (
    id BIGSERIAL PRIMARY KEY,
    event_id TEXT NOT NULL UNIQUE,
    attempt_id TEXT NOT NULL UNIQUE,
    request_id TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    account_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    status_code INTEGER NOT NULL,
    error_class TEXT NOT NULL,
    tokens_in INTEGER NOT NULL DEFAULT 0,
    tokens_out INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    usage_source TEXT NOT NULL,
    partial BOOLEAN NOT NULL DEFAULT false,
    occurred_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS usage_ledger_tenant_occurred_idx
    ON usage_ledger (tenant_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS usage_ledger_account_occurred_idx
    ON usage_ledger (account_id, occurred_at DESC);

CREATE TABLE IF NOT EXISTS migration_runs (
    id TEXT PRIMARY KEY,
    source_system TEXT NOT NULL,
    source_snapshot_at TIMESTAMPTZ,
    status TEXT NOT NULL DEFAULT 'running',
    digest TEXT NOT NULL DEFAULT '',
    dry_run BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS migration_records (
    id BIGSERIAL PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES migration_runs(id) ON DELETE CASCADE,
    source_system TEXT NOT NULL,
    source_id TEXT NOT NULL,
    record_kind TEXT NOT NULL,
    source_digest TEXT NOT NULL,
    raw_summary JSONB NOT NULL DEFAULT '{}'::jsonb,
    conversion_result JSONB NOT NULL DEFAULT '{}'::jsonb,
    target_id TEXT,
    rejection_reason TEXT,
    status TEXT NOT NULL DEFAULT 'imported',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT migration_records_source_key UNIQUE (source_system, source_id, record_kind)
);

CREATE INDEX IF NOT EXISTS migration_records_run_idx ON migration_records (run_id);
