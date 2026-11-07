CREATE TABLE users (id text PRIMARY KEY, email text UNIQUE NOT NULL, password_hash text NOT NULL);
CREATE TABLE tenants (id text PRIMARY KEY, name text NOT NULL);
CREATE TABLE memberships (tenant_id text REFERENCES tenants(id), user_id text REFERENCES users(id), role text NOT NULL CHECK(role IN ('owner','admin','member','viewer')), PRIMARY KEY(tenant_id,user_id));
CREATE TABLE account_tokens (hash text PRIMARY KEY, user_id text NOT NULL REFERENCES users(id), expires_at timestamptz NOT NULL);
CREATE TABLE devices (id text PRIMARY KEY, user_id text NOT NULL REFERENCES users(id), tenant_id text NOT NULL REFERENCES tenants(id), application_id text NOT NULL, name text NOT NULL, revoked boolean NOT NULL DEFAULT false);
CREATE TABLE device_requests (hash text PRIMARY KEY, user_code text UNIQUE NOT NULL, application_id text NOT NULL, name text NOT NULL, expires_at timestamptz NOT NULL, next_poll_at timestamptz NOT NULL DEFAULT now(), device_id text REFERENCES devices(id), consumed boolean NOT NULL DEFAULT false);
CREATE TABLE access_tokens (hash text PRIMARY KEY, device_id text NOT NULL REFERENCES devices(id), expires_at timestamptz NOT NULL);
CREATE TABLE refresh_tokens (hash text PRIMARY KEY, device_id text NOT NULL REFERENCES devices(id), expires_at timestamptz NOT NULL, used boolean NOT NULL DEFAULT false);
CREATE INDEX access_tokens_expiry ON access_tokens(expires_at);
CREATE INDEX refresh_tokens_device ON refresh_tokens(device_id);
CREATE TABLE sync_heads (tenant_id text NOT NULL REFERENCES tenants(id), application_id text NOT NULL, cursor bigint NOT NULL DEFAULT 0, PRIMARY KEY(tenant_id,application_id));
CREATE TABLE sessions (tenant_id text NOT NULL REFERENCES tenants(id), application_id text NOT NULL, id text NOT NULL, owner_id text NOT NULL REFERENCES users(id), writer_id text NOT NULL REFERENCES devices(id), title text NOT NULL, visibility text NOT NULL CHECK(visibility IN ('private','shared')), last_seq bigint NOT NULL DEFAULT 0, PRIMARY KEY(tenant_id,application_id,id));
CREATE TABLE events (tenant_id text NOT NULL, application_id text NOT NULL, session_id text NOT NULL, seq bigint NOT NULL, data jsonb NOT NULL, PRIMARY KEY(tenant_id,application_id,session_id,seq), FOREIGN KEY(tenant_id,application_id,session_id) REFERENCES sessions(tenant_id,application_id,id));
CREATE TABLE changes (tenant_id text NOT NULL, application_id text NOT NULL, cursor bigint NOT NULL, session_id text NOT NULL, last_seq bigint NOT NULL, PRIMARY KEY(tenant_id,application_id,cursor), FOREIGN KEY(tenant_id,application_id,session_id) REFERENCES sessions(tenant_id,application_id,id));
CREATE TABLE receipts (tenant_id text NOT NULL, application_id text NOT NULL, device_id text NOT NULL REFERENCES devices(id), operation_id text NOT NULL, digest text NOT NULL, ack jsonb NOT NULL, PRIMARY KEY(tenant_id,application_id,device_id,operation_id));
-- The application connects as a separate NOSUPERUSER NOBYPASSRLS role.
-- Identity tables are control-plane data; every query there uses authenticated IDs.
-- Data-plane tables additionally enforce tenant + app scope inside transactions.
ALTER TABLE sync_heads ENABLE ROW LEVEL SECURITY;
ALTER TABLE sync_heads FORCE ROW LEVEL SECURITY;
CREATE POLICY scope ON sync_heads USING (tenant_id=current_setting('app.tenant_id',true) AND application_id=current_setting('app.application_id',true));
ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY scope ON sessions USING (tenant_id=current_setting('app.tenant_id',true) AND application_id=current_setting('app.application_id',true));
ALTER TABLE events ENABLE ROW LEVEL SECURITY;
ALTER TABLE events FORCE ROW LEVEL SECURITY;
CREATE POLICY scope ON events USING (tenant_id=current_setting('app.tenant_id',true) AND application_id=current_setting('app.application_id',true));
ALTER TABLE changes ENABLE ROW LEVEL SECURITY;
ALTER TABLE changes FORCE ROW LEVEL SECURITY;
CREATE POLICY scope ON changes USING (tenant_id=current_setting('app.tenant_id',true) AND application_id=current_setting('app.application_id',true));
ALTER TABLE receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE receipts FORCE ROW LEVEL SECURITY;
CREATE POLICY scope ON receipts USING (tenant_id=current_setting('app.tenant_id',true) AND application_id=current_setting('app.application_id',true));
