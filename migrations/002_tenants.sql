-- A6 tenant identity. Gateway never reads these tables: control publishes the
-- active token hashes to Redis (snapshot.PublishTokens) and gateway derives
-- AuthContext from that read-only snapshot. Raw tokens are never stored.
CREATE TABLE IF NOT EXISTS tenants (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    source_system TEXT NOT NULL DEFAULT '',
    source_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT tenants_status_check CHECK (status IN ('active', 'suspended'))
);

CREATE UNIQUE INDEX IF NOT EXISTS tenants_source_key
    ON tenants (source_system, source_id) WHERE source_system <> '';

CREATE TABLE IF NOT EXISTS principals (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    kind TEXT NOT NULL DEFAULT 'user',
    status TEXT NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT principals_status_check CHECK (status IN ('active', 'suspended'))
);

CREATE INDEX IF NOT EXISTS principals_tenant_idx ON principals (tenant_id);

CREATE TABLE IF NOT EXISTS tenant_tokens (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    principal_id TEXT NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    "group" TEXT NOT NULL DEFAULT 'default',
    status TEXT NOT NULL DEFAULT 'active',
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT tenant_tokens_status_check CHECK (status IN ('active', 'revoked'))
);

CREATE INDEX IF NOT EXISTS tenant_tokens_tenant_idx ON tenant_tokens (tenant_id, status);
