ALTER TABLE sessions ADD COLUMN revision bigint NOT NULL DEFAULT 1;
ALTER TABLE sessions ADD COLUMN tags jsonb NOT NULL DEFAULT '[]';
ALTER TABLE sessions ADD COLUMN metadata jsonb NOT NULL DEFAULT '{}';
ALTER TABLE sessions ADD COLUMN archived boolean NOT NULL DEFAULT false;
ALTER TABLE sessions ADD COLUMN deleted boolean NOT NULL DEFAULT false;
CREATE INDEX sessions_owner ON sessions(tenant_id,application_id,owner_id);
-- Metadata and tombstones use the same ordered feed and idempotency ledger as events.
-- Deletion retains the identifier and receipt history so retries cannot resurrect it.
