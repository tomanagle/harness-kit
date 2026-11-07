# Sprint integration validation

The repeatable integration runner lives in `~/projects/sprint`. It rebuilds the
local kit, starts two independent Go/Postgres server installations, and runs the
real Sprint Electron application with an isolated data directory and fake agent.

```sh
cd ~/projects/sprint
npm run smoke:sync
```

Docker must be running. The runner selects free loopback ports, uses uniquely
named Compose projects, and removes its containers, volumes, and Electron profile
when finished. It never connects to production or calls a paid model.

## Covered path

1. The real renderer creates threads through preload/IPC. It exercises prompts,
   settings, folders/files, approvals, cancellation, subagents, manual board moves,
   column rules, rename, and transcript reopening.
2. The shipped **Servers & sync** UI adds a server, registers an account, selects
   its personal tenant, authorizes a device, shares a local thread, displays the
   cached transcript, and renames it through revisioned metadata.
3. Credentials are encrypted with Electron's asynchronous OS-backed `safeStorage`
   API in the main process. Tokens never enter renderer state or kit preferences.
4. The execution device enables remote control and persists its lease epoch.
   A prompt queued from the synced view travels through Postgres command delivery
   to the original sidecar; its response returns through the cached event feed.
5. The original sidecar closes. A fresh kit client and `SprintService` reopen the
   persisted board/transcript. The automatic bridge reads durable-only history.
6. One account connects to Personal and Work tenants; another independent server
   has its own account. Offline Work uploads queue while the other server continues.
   Adding a server alone never uploads local sessions.
7. A real Postgres upload commits, then the test drops its HTTP acknowledgement.
   Sidecar, registry, and sync-store instances restart. The persisted operation ID
   produces no duplicate events or changes. Cache pages match original events.
8. A second device replays messages, approvals, cancellation, and subtasks using
   the same conversation projection as the UI. Runtime pause/resume uses unified
   settings; a session's destination cannot be rebound by switching tenants.
9. Revisioned metadata updates succeed; stale revisions fail. Tenant/application/
   server boundaries, a foreign writer, and per-device revocation are checked.

The connection UI also exposes tenant member administration, tags/column metadata,
archive/delete, command status, conflicting-edit resolution, pause, and disconnect.
These additional UI controls are not all exercised by the Electron smoke. Server
integration tests cover their authorization, revisions, tombstones and execution
fencing. Unit tests cover encrypted-store reopening, capacity bounds, expired
leases, lost command acknowledgements, checkpoint/cache failure and Go parity.

## Boundaries

The UI test uses the real macOS secure-storage API; unit tests inject encryption
for repeatable failure cases. The separate multi-server bridge test uses an
in-memory credential adapter. It tests sidecar/client restart, not a second
physical machine or a full OS-keychain migration. Network loss is injected at the
client fetch boundary; database commits, grants, disk writes, and replay are real.

The main local kanban board remains the execution workspace. Synced sessions are
shown in the separate account/tenant-scoped view, with editable namespaced column
metadata. Native CLI history and files stay on the execution device. The test
uses a fake agent and establishes the harness wiring, not provider availability
or cross-device portability of native runtimes.

Independent server tests exercise three repositories/pools/HTTP instances sharing
only Postgres, losing the accepting replica after commit, simultaneous retries,
refresh/revocation on other replicas, lease expiry, and uncertain commands. The
[k6 fixture](../load/README.md) adds three actual containers behind nginx with
thresholded sync/replay/auth/long-poll workloads. The restore smoke copies the
fixture database and compares all public-table contents. These are reproducible
correctness checks, not a claim of production capacity or an external security audit.
