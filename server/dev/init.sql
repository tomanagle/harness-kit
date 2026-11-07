-- Development-only credentials. Production provisioning supplies separate
-- migration and runtime identities through the deployment secret manager.
CREATE ROLE harness_app LOGIN PASSWORD 'harness_dev' NOSUPERUSER NOBYPASSRLS;
GRANT CONNECT ON DATABASE harnesskit TO harness_app;
GRANT USAGE ON SCHEMA public TO harness_app;
ALTER DEFAULT PRIVILEGES FOR ROLE harness_owner IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO harness_app;
