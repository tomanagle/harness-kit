# Harness Kit Server

A self-hostable Go/Postgres coordination server for harness applications. This is
the shared control plane for accounts, tenants, device grants, transcripts, and
remote execution coordination. The local SDK continues to work without it.
Application replicas are stateless; all durable coordination lives in Postgres.

The implementation follows the GOTTH shape, with dependency construction kept in
`cmd/main.go`. There is no server dependency in the core SDK.

| Package                                 | Responsibility                                                           |
| --------------------------------------- | ------------------------------------------------------------------------ |
| `internal/config`                       | Load settings through an injected environment reader                     |
| `internal/logger`                       | Construct the `slog` logger using an injected output writer              |
| `internal/database`                     | Open Postgres, check connectivity, validate runtime privileges           |
| `internal/httpserver`                   | Configure HTTP serving, drain requests, close on shutdown deadlines      |
| `internal/auth`                         | Password hashing and tokens using injected entropy                       |
| `internal/store`                        | Application persistence contract                                         |
| `internal/store/dbstore`                | Postgres repository with injected database, passwords, tokens, and clock |
| `internal/handlers`                     | Account, device, session, health, and web routes                         |
| `internal/tenants`                      | Role permissions                                                         |
| `internal/templates`                    | Templ account/device pages                                               |
| `internal/wire`, `internal/contractgen` | Versioned contract and client/schema generation                          |
| `migrations`                            | Embedded, versioned SQL migrations                                       |

Every fallible constructor has an ordinary error-returning form and a `Must...`
startup wrapper. Only `Must...` functions may panic, and only `func main` may call
or reference those wrappers. Runtime operations, including random token generation
and schema generation, return errors. Tests use the ordinary constructors.

Dependencies stay private after construction. Repositories consume a narrow
Postgres interface; migration code only needs transaction creation. Logging uses
Go's `slog.Logger` and `slog.Handler` rather than a parallel logger abstraction.
`internal/architecture` enforces the panic/Must rules and keeps pool construction
and entropy ownership at their intended boundaries. See [AGENTS.md](AGENTS.md)
for the server conventions.

## Run locally

From the repository root:

```sh
make server-dev
```

Set `HARNESS_SERVER_PORT` or `HARNESS_POSTGRES_PORT` if the default ports are in use.

This starts Postgres, applies migrations using a separate schema owner, and starts
the server at http://localhost:8080. The development credentials and exposed ports
are local-only. Open `/login` to register, then approve a device at `/device`.
Tenant creation and member management currently use the typed API, not an admin UI.

Building the server requires Go 1.27.3 or newer. The core and Go client retain their
existing Go version requirements.

Without Docker, provision two database roles: a schema owner for migrations and a
runtime role with `CONNECT`, schema `USAGE`, and table `SELECT/INSERT/UPDATE/DELETE`.
Grant default privileges for future tables too. The runtime role must not own
schema tables, be a superuser, or have `BYPASSRLS`; startup checks this.

```sh
cd server
# Supply DATABASE_URL through your environment/secret manager for each role.
# First use the migration role:
make migrate
# Then use the runtime role:
PUBLIC_URL=http://localhost:8080 ALLOW_REGISTRATION=true make run
```

`LISTEN_ADDR` defaults to `127.0.0.1:8080`. `PUBLIC_URL` must be an HTTPS origin,
except loopback development. Registration is disabled unless explicitly enabled.
The production container uses the same binary; TLS termination and database
credentials come from deployment configuration. Logs use Go's `slog` JSON handler
on stderr; `LOG_LEVEL` controls its level. `DATABASE_MAX_CONNECTIONS` controls the
pool limit (default 20). `/health/live` and `/health/ready` support health checks.

## What works

- Account registration/login, Argon2id password hashing, and a personal tenant.
- Multiple tenant memberships per account. Owner/admin/member/viewer roles map to
  named permissions. Members can publish; viewers can read shared sessions.
  Administrators cannot read another user's private sessions just because of role.
- Listing members, adding existing accounts, changing roles, and removing members. Owners cannot be
  removed through this endpoint; ownership transfer needs a separate workflow.
- Browser device-code approval with explicit tenant selection and application/device
  labels. Grants are scoped to one account, tenant, application, and device.
- Fifteen-minute access tokens, rotating thirty-day refresh credentials, and
  revocation. Tokens are hashed in Postgres. Reusing a rotated refresh token revokes
  that grant. Removing membership invalidates existing access on the next request.
- Session creation and append-only event batches, explicit private/shared visibility,
  and a fenced writing device. Another authorized device can replay the transcript.
- Revisioned titles, tags, archive state, application metadata, and deletion tombstones.
  Stale edits conflict instead of overwriting newer data. Deletion removes events
  and commands; tombstones and idempotency receipts remain for safe offline replay.
- Explicit execution leases with monotonically increasing epochs, durable commands,
  and at-most-once claiming. Expired ownership marks unresolved commands uncertain.
- Device inventory and revocation, account logout, optional verified email, password
  reset, distributed authentication rate limiting, metadata-only session audit events,
  and a replica-safe SMTP outbox and cleanup command.
- Idempotent upload receipts, expected sequence checks, bounded replay pages, and
  resumable long polling. Tenant/application cursors serialize through a locked row,
  so a later commit cannot hide an earlier uncommitted change behind the cursor.
- Postgres row-level security for tenant/application data, plus account visibility
  checks. Authorization is rechecked inside sync transactions and fenced against
  concurrent revocation.
- Generated TypeScript and Go HTTP clients, and a JSON Schema contract.

Application IDs are namespace labels supplied by the harness, not verified OAuth
client identities. The current device flow uses the kit's JSON HTTP contract; it
is not advertised as a general-purpose OAuth authorization server.

## Integrate a harness

Server preferences use the kit's **existing settings document** and can change at
runtime. Credentials are injected separately; use an OS keychain or your existing
secure credential service. The client never stores them in settings or the outbox.

```ts
import { createHarnessKit } from "@tomanagle/harness-kit";
import { stdio, fileSyncStore } from "@tomanagle/harness-kit/node";

const kit = await createHarnessKit({
  transport: stdio({
    command: "harness-kit",
    settings: { process: { store: sessionsDirectory, settingsPath: preferencesPath } },
  }),
  servers: {
    applicationId: "sprint",
    credentials: secureCredentials, // implements get(key), set(key, grant), delete(key)
    syncStore: fileSyncStore(syncDirectory),
  },
});

await kit.servers.add({
  id: "work",
  name: "Work",
  url: "https://sync.company.example",
  enabled: true,
});
await kit.servers.add({
  id: "personal",
  name: "Personal",
  url: "https://sync.example.com",
  enabled: true,
});

const work = await kit.servers.authorize("work", {
  onCode: ({ verificationUri, userCode }) => showDeviceApproval(verificationUri, userCode),
});
// Persist work.scope's nonsecret accountId and tenantId in your application's UI state.
// On restart: kit.servers.connect("work", { accountId, tenantId }).

// Opt in an existing local session. This destination is persisted and immutable.
await work.sync.publish(session.id, {
  title: "Review the build",
  visibility: "private",
});
const unsubscribe = work.sync.onStatus((status) => showSyncStatus(work.scope, status));
work.sync.start(); // On restart: connect the same scope and call start() again.

// Rebuild a remote view without restoring or starting an agent runtime.
const remoteSessions = await work.sync.sessions();
const page = await work.sync.eventsPage(session.id, { after: 0, limit: 50 });
// Each item is { seq, data }; data is the original Harness Kit event.

// The same settings object controls startup and runtime. No restart required.
await kit.settings.configure({
  patch: { sync: { paused: false, pollIntervalMs: 1000, batchEvents: 25, workPerCycle: 8 } },
});
// Stop just this scope, or close all workers and the sidecar with kit.close().
await work.sync.stop();
unsubscribe();
```

The registry keeps clients bound to server URL + account + tenant + application.
Changing a tab's selected tenant cannot redirect background work. Removing,
disabling, or changing a server setting stops subsequent requests from existing
connections. In-flight requests may finish. Removal does not revoke grants or erase
queued content; call `work.revoke()` first if that is the intended action.

The worker reads only `session.durableEventsPage`, persists an immutable pending
operation before HTTP, and atomically commits its outgoing checkpoint after the
acknowledgement. Failed operations retain their IDs across restarts. Incoming
pages are durable before their change-feed cursor advances. Upload failures in
one bound session do not prevent another session's upload attempt. Independent
server/tenant workers do not share cursors or queues.

`fileSyncStore` keeps transcripts in 25-event disk chunks. Reads are limited to
100 events and about 1 MiB; it never loads the full transcript. Its injected store
options bound checkpoint/index bytes (32 MiB by default), sessions per scope
(1000), and bytes per cached session (256 MiB). Capacity exhaustion reports a
blocked status and preserves checkpoints; it does not silently evict history.
These are storage-adapter limits, not a process RSS guarantee. There is no global
disk quota or automatic cache eviction yet. Implement `SyncStore` to use another
storage system. One registry/process owns each store directory and credential
namespace; separate devices/processes need separate directories.

`sync.paused`, `sync.pollIntervalMs` (100–60000), `sync.batchEvents` (1–100), and
`sync.workPerCycle` (1–100) use the unified settings API and take effect on the
next cycle. Defaults are false, 1000, 25, and 8. A cycle attempts at most that many
local sessions and that many incoming pages. Retries back off up to 30 seconds.
The worker currently polls; it does not use the server's long-poll API. Statuses
include `paused`, `syncing`, `idle`, `offline`, `authentication_required`, `blocked`,
and `stopped`, with safe reason codes suitable for a UI. A completed pass is
`idle`; it may still have more pages to process on subsequent cycles.

`fileSyncOutbox`, `enqueue`, `flush`, `changes`, and the generated protocol remain
available for hosts that want to own scheduling and batching. Do not mix manual
uploads and the automatic worker for the same session. `syncOnce()` runs one
bounded pass for a host-owned scheduler; `start()` owns retries until `stop()`.

A lost refresh-token response requires reauthorization; clients must not repeatedly
retry old refresh credentials. Concurrent credential rotation across separate
processes is not supported yet. Reauthorization currently creates a new device grant:
it can read existing sessions, but cannot append to sessions owned by the previous
grant. Explicit writer recovery/transfer uses the lease API; queued operations remain
on disk rather than being silently reassigned. A replacement grant does not
automatically take over local native runtime history.

Adding a server never uploads a local session. Only explicitly published sessions
are uploaded automatically. The initial visibility remains fixed; titles and other mutable metadata use
`sync.update()` with an expected revision. The incoming cache is read-only execution
history; native agent state and filesystem state are not made portable by sync.

The durable-only core API is also generated for Go. In-process Go callers use
`Manager.DurableEventsPage`; custom persistent stores implement
`DurableEventPageStore`. Memory-only stores and live persistence failures return
`durable_history_unavailable`. Gaps block automatic replication. The ordinary UI
history API retains its existing behavior of merging unsaved visible events.

For lower-level access use `remoteClient` from `@tomanagle/harness-kit/remote`, or
`remote.New` from `github.com/tomanagle/harness-kit/packages/client-go/remote`.
The Go package also provides `Authorize`, `NewConnection` with an injected
credential store, and `NewSync` / `NewFileSyncStore` for automatic durable sync.
Pass the generated sidecar client as `Source`, a scope-bound remote client as
`Protocol`, and a `Settings` function reading the same unified sync settings.
`Publish` opts in a session, `Once` runs a bounded pass, and `Run(ctx)` owns retries
until cancellation. `Update`, `Delete`, and `Command` persist operations first.
The host chooses credential storage; no plaintext credential adapter is supplied.
Event replay returns `nextAfter` and `hasMore`; advance using `nextAfter` until
`hasMore` is false. `lastSeq` is the session head, not the next page cursor.

Both are generated from `internal/wire` alongside `protocol/server-v1.schema.json`.
Run `make generate` after contract changes.

## Verification

```sh
# Administrator access to an explicitly disposable test Postgres service.
# Each run creates a random database and non-bypass runtime role, then removes them.
HK_TEST_DATABASE_URL=... make test-server
make check-generated
make -C server security
```

CI starts real Postgres and runs the race-enabled integration tests. They cover
multiple devices, tenants, applications, private/shared visibility, role enforcement,
parallel idempotent retries, cursor pagination, refresh reuse, membership removal,
and grant revocation. Client tests cover disk recovery after a lost acknowledgement,
credential rotation concurrency, and live configuration changes.

The [Sprint validation report](docs/sprint-validation.md) describes the repeatable
Electron-to-server test, its failure-injection checks, and the gaps it confirmed.
Run it from the sibling Sprint project with `npm run smoke:sync`.

## Metadata and remote execution

```ts
const current = await work.protocol.getSession({ sessionId });
await work.sync.update({
  sessionId,
  expectedRevision: current.revision,
  title: "Ready for review",
  tags: ["review"],
  archived: false,
  metadata: { sprint: { column: "Review" } },
});
// A network retry retains its operation ID. A 409 requires an explicit merge.
const pending = await work.sync.pending();
// After inspecting a rejected edit: discardMutation(id), then queue a merged edit.

const id = await work.sync.command({
  sessionId,
  epoch: current.writerEpoch,
  kind: "prompt",
  payload: { text: "Run the tests" },
});
// Once sent, work.protocol.getCommand({id}) reports admission status.
```

The execution device opts in with `serveExecution` from the remote entry point.
Its injected `acquired(lease)` callback persists the epoch with `sync.useLease`
before commands can run. `handle(command, signal)` durably admits commands using
`command.id` as a turn/deduplication ID; it returns after admission so cancel and
approval commands can continue. `lost(reason)` must stop local execution and wait
for it to stop. The helper renews ownership, enforces an expiry deadline, and
retries acknowledgements without rerunning actions. Only then can `stop()` release
a healthy lease. These host hooks are deliberately independent of an agent runtime.

A completed command means **host admission**, not a completed agent turn. Read the
transcript for turn completion. An uncertain outcome must be reconciled by the
host/user; it is never automatically replayed. Leases fence server writes, but
cannot undo a tool action already sent to an external service. Native CLI history,
worktrees, and runtime state stay on the execution device. Sprint routes remote
prompts to that device; it does not pretend transcript replay migrates a runtime.

For Electron, `encryptedCredentialStore(directory, safeStorage)` from the Node
entry point uses the asynchronous OS-backed encryption API. It refuses unavailable
key storage and Linux's `basic_text` fallback. `scopes()` supplies nonsecret scope
identifiers for reconnecting on startup. Keep it in the main process.

## Horizontal deployment and operations

See [deployment and recovery](docs/operations.md) and the [k6 guide](load/README.md).
`make server-cluster-up` starts three interchangeable replicas behind a proxy;
`make server-load-test` runs integrity and latency checks without sticky routing.
The same binary supports self-hosted and cloud deployments. Cloud provisioning,
billing, SSO, and a hosted control-panel product are separate from this server.

This implementation supports the documented account/device/sync/execution flows.
It does not automatically migrate native runtime state, sync arbitrary filesystem
attachments, change existing session visibility, or prune change history. Cached
views need an online authorization check before displaying newly restricted data;
revocation cannot erase a copy already downloaded to another device. Use the
explicit scope separation and credential revocation APIs in the harness UI.

Before a public deployment, configure TLS, trusted proxy ranges, verified email,
registration policy, database backups, monitoring, and workload-specific limits.
The integration and load tests are reproducible checks, not a claim of unlimited
capacity or a completed independent security review.
