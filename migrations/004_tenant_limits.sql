-- A10 per-tenant limits. 0 means unlimited. Published to gateway inside the
-- token snapshot (snapshot.TokenRecord); gateway enforces them with the same
-- atomic Redis script it uses for per-account limits.
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS max_concurrency INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS rpm INTEGER NOT NULL DEFAULT 0;
