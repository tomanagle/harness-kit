# Deployment and recovery

The Go application has no durable local state and needs no sticky sessions. Every
replica uses the same Postgres database. Account/device credentials, refresh-token
rotation, tenant roles, rate limits, session revisions, event sequences, operation
receipts, execution epochs, commands, and mail jobs are database-backed. No Redis
or shared filesystem is required. Local HTTP connections, bounded password slots,
and polling timers are disposable. A reconnect supplies its persisted cursor.

Run migrations once as a deployment job before starting the new application
version. Migrations use an advisory lock and transaction. The separate runtime
role must be a non-owner, non-superuser, `NOBYPASSRLS` role. Startup rejects an
unsafe runtime role. Use the role/grant pattern in `dev/init.sql`, with production
credentials delivered by your secret manager. Never use fixture passwords.

Place replicas behind TLS termination at the configured `PUBLIC_URL`. Disable
request/response body and Authorization-header logging at the proxy. Keep database
ports private. Set `TRUSTED_PROXY_CIDRS` to **only** your actual proxy ranges and
make that proxy overwrite `X-Real-IP`; untrusted forwarding headers are ignored.
There is no forwarded-header trust by default. A broad private-network range in
the disposable Compose fixture is not a production recommendation.

`/health/live` reports process liveness; `/health/ready` checks the database.
Send SIGTERM and allow the HTTP drain deadline before killing a replica. Retries
must preserve operation IDs; even if a committed response is lost, another replica
returns the receipt. Database transaction locking orders cursor commits, so a
reader cannot skip a late commit. Keep all replicas on a compatible schema/API
version during rolling deployments.

## Configuration

| Variable                         | Purpose/default                                                             |
| -------------------------------- | --------------------------------------------------------------------------- |
| `DATABASE_URL`                   | Required, runtime database credential; migration job uses owner credential  |
| `DATABASE_MAX_CONNECTIONS`       | Pool per replica, default 20                                                |
| `LISTEN_ADDR`                    | Default `127.0.0.1:8080`; containers use `0.0.0.0:8080`                     |
| `PUBLIC_URL`                     | External HTTPS origin; loopback HTTP allowed for development                |
| `ALLOW_REGISTRATION`             | Default false                                                               |
| `AUTH_REQUESTS_PER_MINUTE`       | Shared per-address/per-auth-route limit, default 120                        |
| `TRUSTED_PROXY_CIDRS`            | Comma-separated explicit proxy prefixes, default empty                      |
| `REQUIRE_VERIFIED_EMAIL`         | Default false; true requires SMTP and verified email before device approval |
| `SMTP_ADDR`, `SMTP_FROM`         | Mail host:port and sender address                                           |
| `SMTP_USERNAME`, `SMTP_PASSWORD` | Optional SMTP authentication                                                |
| `SMTP_INSECURE_LOOPBACK`         | Development only, defaults false; rejected for remote hosts                 |
| `LOG_LEVEL`                      | Structured slog level                                                       |

Server infrastructure configuration is supplied at process startup; roll replicas
to change it. Harness connection/sync preferences use the kit's unified settings
object and can change at runtime without restarting the sidecar.

SMTP requires STARTTLS with certificate verification. `/account/recover` requests
verification/reset email; links expire in 30 minutes and are single-use. A reset
revokes every account login and device grant for the account. Public responses do
not reveal whether an email exists. Mail delivery uses row locks with SKIP LOCKED
across replicas. A crash after SMTP acceptance can resend the **same link**; it
cannot apply the reset twice. Failed sends retry after a minute, up to ten attempts.
Monitor mail delivery errors. The pending mail body contains the emailed secret
until delivery or expiry; protect the database and backups accordingly.

## Capacity and retention

Budget total database connections across all replicas, migration/maintenance jobs,
and monitoring. Scaling HTTP replicas does not scale the database. A hot
tenant/application serializes its cursor row. Auth concurrency is four password
operations per replica; Argon2 memory plus HTTP/query buffers contributes to RSS.
Set container memory limits and Go `GOMEMLIMIT` appropriately for measured load.
Use the [k6 workloads](../load/README.md), including a single hot tenant and long
polls, before setting an autoscaling policy. Measure pool waits, query/lock latency,
CPU, RSS, request failures, and backlog growth on representative infrastructure.

Run `harness-kit-server cleanup` periodically using the runtime role. Each run
removes up to 5,000 expired rows per authentication table and expired mail jobs,
and old rate-limit keys. Concurrent runs coordinate with an advisory lock. It
does not delete session history, audit records, change cursors, or idempotency
receipts: arbitrary pruning would invalidate offline clients' retry guarantees.

Deleting a session removes its event and command payloads and publishes a tombstone.
Clients durably hide the cached transcript before deleting event chunks. Session
identity, metadata, audit records and receipts remain. This is shared-transcript
deletion, not an account-erasure API. Backups follow the operator's retention
policy. Visibility is immutable; changing it requires a future cache invalidation
protocol. Authorization revocation stops future reads; it cannot erase previously
downloaded data from an untrusted device.

## Backup and disaster recovery

Use your Postgres service's encrypted backups and WAL/PITR, with a tested recovery
point objective and recovery time objective. Back up the **whole database**;
restoring events without receipts, cursors, identities, and execution epochs breaks
sync guarantees. Preserve role grants and migration versions, and protect credentials.

For a logical restore rehearsal in an isolated environment:

1. Take a consistent `pg_dump -Fc` as an authorized backup role.
2. Restore into a fresh database using `pg_restore --no-owner`, provision the runtime
   role/grants, and run the migration command.
3. Compare session/event/receipt counts and payloads, test login/device refresh,
   replay a saved cursor, and retry a committed upload with its original operation ID.
4. Start two replicas against the restored database and rerun isolation/failover tests.

A restore to an **older** point loses acknowledged writes after that point. Do not
silently attach existing clients with newer checkpoints. Quiesce writers, revoke
old device grants, and reconnect clients under a new server identity/cache namespace;
reconcile durable local histories into new sessions explicitly. There is no automatic
rollback/reset protocol. Preserve the original databases and local histories until
reconciliation completes. A backup is not verified until a restore drill succeeds.

The disposable test fixture includes `make server-restore-test` to dump and restore
its database into a separate temporary database and compare data fingerprints.
It never restores over the source database and removes only its temporary copy.
