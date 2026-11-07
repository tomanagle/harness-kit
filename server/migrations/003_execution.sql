ALTER TABLE sessions ADD COLUMN writer_epoch bigint NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN lease_until timestamptz;
CREATE TABLE commands (
 tenant_id text NOT NULL, application_id text NOT NULL, session_id text NOT NULL,
 id text NOT NULL, requester_id text NOT NULL REFERENCES users(id),
 epoch bigint NOT NULL, kind text NOT NULL CHECK(kind IN ('prompt','cancel','approval')),
 payload jsonb NOT NULL, status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','claimed','completed','failed','uncertain')),
 result jsonb NOT NULL DEFAULT '{}', created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,application_id,id),
 FOREIGN KEY(tenant_id,application_id,session_id) REFERENCES sessions(tenant_id,application_id,id)
);
CREATE INDEX commands_pending ON commands(tenant_id,application_id,session_id,created_at,id) WHERE status='pending';
ALTER TABLE commands ENABLE ROW LEVEL SECURITY;
ALTER TABLE commands FORCE ROW LEVEL SECURITY;
CREATE POLICY scope ON commands USING (tenant_id=current_setting('app.tenant_id',true) AND application_id=current_setting('app.application_id',true));
-- Audit rows contain identifiers and operation names, never prompts or credentials.
CREATE TABLE audit_events (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), tenant_id text NOT NULL, application_id text NOT NULL,
 account_id text NOT NULL, device_id text NOT NULL, session_id text NOT NULL,
 action text NOT NULL, occurred_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE audit_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_events FORCE ROW LEVEL SECURITY;
CREATE POLICY scope ON audit_events USING (tenant_id=current_setting('app.tenant_id',true) AND application_id=current_setting('app.application_id',true));
CREATE FUNCTION audit_session_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO audit_events(tenant_id,application_id,account_id,device_id,session_id,action)
 VALUES(NEW.tenant_id,NEW.application_id,coalesce(current_setting('app.account_id',true),''),coalesce(current_setting('app.device_id',true),''),NEW.id,
 CASE WHEN TG_OP='INSERT' THEN 'session.created' WHEN NEW.deleted AND NOT OLD.deleted THEN 'session.deleted' WHEN NEW.writer_epoch!=OLD.writer_epoch THEN 'execution.acquired' WHEN NEW.revision!=OLD.revision THEN 'session.updated' ELSE 'session.events' END);
 RETURN NEW;
END;
$$;
CREATE TRIGGER audit_session AFTER INSERT OR UPDATE OF revision,last_seq,writer_epoch ON sessions FOR EACH ROW EXECUTE FUNCTION audit_session_change();
CREATE TABLE rate_limits (key text PRIMARY KEY, window_start timestamptz NOT NULL, count integer NOT NULL);
