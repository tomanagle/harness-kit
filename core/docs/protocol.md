# Sidecar protocol (version 1)

The `harness-kit` binary speaks JSON-RPC 2.0 over its stdin and stdout.
Electron, Wails, CLIs and services all use this same protocol; no HTTP
server and no network port is involved. The server's Go wire definitions in
[`rpc/protocol`](../rpc/protocol/protocol.go) and their referenced domain types
produce a shared [JSON Schema](../../protocol/schema.json). That schema generates
the wire types and RPC methods in both [`packages/client-ts`](../../packages/client-ts)
and [`packages/client-go`](../../packages/client-go). Transport handling and
application helpers remain handwritten.

Run `make generate` from the repository root after changing the protocol, then
commit the schema and generated clients together. `make check-generated` and CI
reject stale output. See the [generation guide](../../protocol/README.md) for
the source files and workflow. This document describes protocol behavior such as
ordering, cancellation and replay, which a data schema cannot express alone.

## Framing

- One JSON-RPC message per line, UTF-8, terminated by `\n` (`\r\n` is
  accepted). JSON string escaping guarantees messages contain no raw
  newlines.
- Server messages always include `"jsonrpc": "2.0"`. Client messages
  should include it; the server tolerates its absence.
- Batch requests (JSON arrays) are rejected with `-32600`.
- Lines larger than 32 MiB are discarded and answered with `-32700`; the
  connection stays open.
- **stdout carries protocol messages only.** The server writes its logs to
  stderr (`--log-level`).

## Lifecycle

1. The client spawns `harness-kit serve` (or just `harness-kit`) and sends
   `initialize`. Requests sent before it fail with `-32002 not_initialized`.
   Requests sent after it are processed after it completes, even if the
   client pipelines them without awaiting the response. A second
   `initialize` fails with `-32600`.
2. The server may send `event` notifications as soon as it has received
   `initialize`, possibly before the response.
3. To stop, the client sends `shutdown`, which closes every session and its
   runtime processes, and then closes the server's stdin.
4. **The server exits when stdin closes**, for whatever reason. If the
   parent process crashes, the OS closes the pipe; the sidecar closes all
   sessions, kills runtime process trees and exits. SIGINT and SIGTERM do
   the same.

Runtime processes run in their own process groups (Unix) and are
terminated as a tree with `taskkill /T` (Windows), so closing a session
terminates everything the runtime started.

## Versioning

`initialize.params.protocolVersion` is required. The server accepts only
versions it implements and otherwise returns `-32008
unsupported_version` with `data.supportedVersions`. The version is a
single integer that changes only on breaking changes. These are
non-breaking and happen without a version change:

- new methods
- new optional params or result fields
- new event types
- new `runtime.status` values
- new error `reason` values

Clients must ignore unknown fields and unknown event types.

## Methods

All params and results are objects. "→" means the result.

### `initialize`

```json
{"protocolVersion": 1, "clientInfo": {"name": "my-app", "version": "1.2.0"}}
→ {"protocolVersion": 1,
   "serverInfo": {"name": "harness-kit", "version": "v0.1.0"},
   "capabilities": {"runtimes": [RuntimeInfo], "allowUnsafe": false, "eventLogSize": 2048}}
```

### `shutdown`

`{}` → `{}`. Closes all sessions. The client should then close stdin.

### `settings.get`, `settings.configure`, `settings.stats`

`initialize.capabilities.settings` advertises the unified settings API.

```json
settings.get {"target"?: {"sessionId": "ses_…"}}
→ {"target": {}, "revision": 3, "values": Settings, "fields": {"memory.maxCacheBytes": Field}}

settings.configure {
  "target"?: {"sessionId": "ses_…"},
  "patch": {"memory": {"evictAfterIdleMs": 600000}},
  "reset"?: ["memory.maxCacheBytes"],
  "expectedRevision"?: 3
}
→ {"target": {}, "revision": 4, "values": Settings, "fields": {...}, "deferred"?: ["reason"]}

settings.stats {}
→ {"cachedBytes": 0, "cachedEvents": 0, "cachedSessions": 0,
   "residentSessions": 0, "unsavedEvents": 0, "deferred"?: ["reason"]}
```

An omitted target selects global settings. A session target exposes that
session's selections and pin state. Global `session` fields supply defaults for
new sessions; scoped model/effort/native settings apply to future turns.
`Field` describes `type`, `unit`, `minimum`, `nullable`, `scopes`, `when` and
`writable`. Settings and patch rules are specified in
[configuration.md](configuration.md#one-settings-document).

`settings.updated` is a connection-wide notification with the same snapshot
shape. It is separate from per-session sequenced events. Subscribe before
fetching the initial snapshot; revisions are scoped to their target. If a
notification has `resync: true`, the consumer fell behind and must refresh all
displayed targets. No unbounded notification backlog is retained.

Invalid updates apply nothing. Stale revisions return `settings_conflict`
(-32014); changes to startup wiring return `restart_required` (-32015);
startup-only session changes return `new_session_required` (-32016).
Changes requiring restart are not automatically applied after a restart: the
owner updates its saved preferences and supplies the new startup document.

### `logs.subscribe`, `logs.unsubscribe`

`{}` → `{}`. `initialize.capabilities.logs` advertises structured diagnostic
streaming. Subscribe explicitly; ordinary initialization does not enable logs.
Await the subscribe response before starting work to observe it. These methods
are idempotent and ordered per connection, and unsubscribe or connection close
releases that connection's queue without affecting other subscribers.

Notifications use method `log`, separately from session `event` notifications:

```json
{
  "jsonrpc": "2.0",
  "method": "log",
  "params": {
    "record": {
      "seq": 7,
      "time": "2026-11-04T00:00:00Z",
      "level": "INFO",
      "message": "session started",
      "attrs": { "sessionId": "ses_example", "runtime": "fake" }
    }
  }
}
```

`record.source?` is `{function,file,line}` and `record.truncated?` marks an
oversized normalized record. Standard slog groups live inside `attrs`. Each
notification has either `record` or `dropped: {fromSeq,toSeq,count}`, reporting
queue overflow or an envelope too large for the configured limit. Sequences
belong to the log stream and reset after restart. Logs are future-only and not
persisted or replayed through `session.events`.

The unified global `logging` settings apply live: `level` (default `info`),
positive `maxQueueRecords` (256), `maxQueueBytes` (1048576), and `maxRecordBytes`
(65536). Limits apply independently to each subscriber, plus one in-flight
delivery and a constant-size loss marker. See [logging](logging.md) for handler
composition, client examples, and retention semantics.

### `runtime.list`

`{}` → `{"runtimes": [RuntimeInfo]}`. This does no probing.

```json
RuntimeInfo = {"name": "codex", "displayName": "OpenAI Codex",
  "capabilities": {"streaming": true, "approvals": true, "resume": true, "cancel": true, "usage": true, "cost": false,
                   "listSessions": true, "mcp": true, "contextUsage": true, "compaction": true,
                   "configuration": {
                     "model": {"startup": true, "session": true, "turn": true, "live": false},
                     "effort": {"startup": true, "session": true, "turn": true, "live": false},
                     "options.sandbox": {"startup": true, "session": false, "turn": false, "live": false}
                   }}}
```

`capabilities.configuration` describes each setting by name: portable fields
such as `model` and `effort`, startup fields such as `cwd`, and native
options as `options.NAME`. The flags mean:

- `startup`: accepted by `session.create`.
- `session`: accepted by `session.configure` for subsequent turns.
- `turn`: accepted as a `session.prompt` override.
- `live`: an explicit active-turn control is supported. Current adapters
  advertise no live configuration controls.

A missing setting or false flag means that operation is unsupported. A
capability describes the adapter's operation; runtime versions and particular
values may still be rejected during validation. Core does not restart or
cancel a turn to apply a setting. See [configuration.md](configuration.md)
for host wiring, inheritance, and adapter-specific limits.

### `runtime.discover`

`{"runtimes"?: ["codex"]}` → `{"runtimes": [Discovery]}`. The server probes
installation, version and sign-in state using each CLI's own documented
status interface. It never starts an agent turn and never uses model quota.
A probe that fails is reported in `problems`; the call itself still
succeeds.

```json
Discovery = {"runtime": "codex", "installed": true, "path": "/usr/local/bin/codex", "version": "0.166.0",
  "auth": {"state": "authenticated", "method": "chatgpt", "plan": "pro"},
  "ready": true, "problems": [], "notes": ["..."], "capabilities": {...}}
```

`auth.state` is `authenticated`, `unauthenticated` or `unknown`. Personal
data such as email addresses is never included.

### `runtime.sessions`

`{"runtime": "codex", "cwd"?: "/abs", "limit"?: 50}` →
`{"sessions": [RuntimeSession]}`. Lists the runtime's _own_ stored sessions,
newest first, including ones started outside Harness Kit (for Codex: in the
Codex CLI, IDE extension or `codex exec`). It is only available for runtimes
with `capabilities.listSessions`. Other runtimes fail with `-32012
unsupported`; Claude Code exposes listing only through its Python and
TypeScript SDKs.

```json
RuntimeSession = {"id": "019a…", "cwd": "/abs", "title": "Refactor auth", "model": "…",
  "source": "cli", "createdAt": "RFC3339", "updatedAt": "RFC3339"}
```

Pass `id` as `resumeId` to `session.create` to continue it.

### `session.create`

```json
{"runtime": "codex", "cwd": "/abs/path", "model"?: "...", "effort"?: "high", "resumeId"?: "...",
 "instructions"?: "...", "rawEvents"?: false, "options"?: {...}, "metadata"?: {"k": "v"},
 "title"?: "Refactor auth",
 "mcp"?: {"mcpServers": {"github": {"command": "npx", "args": ["..."], "env": {"T": "${T}"}}}, "inheritNative": true}}
→ {"session": SessionInfo}
```

`mcp` adds MCP servers for this session in the portable format described in
[mcp.md](mcp.md). An invalid definition fails with `-32602`. A sidecar
started with `--kit-mcp` also merges the user's config file and a trusted
project `harness-kit.json`. Servers that are skipped (unset `${VAR}`, an
untrusted project file, a name clash) are reported as `runtime.status`
events with `status: "warning"` right after `session.started`.

`title` names the session (see `session.rename` for the rules); without
one, the title becomes the start of the first prompt.

`cwd` must be absolute. `resumeId` is the runtime-native ID from a previous
`SessionInfo.runtimeSessionId`. `options` are runtime-specific (see
[runtimes.md](runtimes.md)); unknown option fields are rejected. Options
that disable sandboxes or approvals fail with `-32009 policy_violation`
unless the sidecar runs with `--allow-unsafe`. Portable `model` and `effort`
use the runtime's own names. Empty selections request native defaults. The
legacy startup option `options.effort` is normalized into portable `effort`;
a nonempty portable value takes precedence. Native option fields are
validated by their adapter.

```json
SessionInfo = {"sessionId": "ses_…", "runtime": "codex", "runtimeSessionId": "019a…",
  "state": "idle", "activeTurnId"?: "…", "cwd": "…", "model": "…", "metadata": {},
  "createdAt": "RFC3339", "updatedAt": "RFC3339", "lastSeq": 1, "error"?: ErrorInfo,
  "title"?: "Refactor auth", "turns": 2, "lastStatus"?: "completed",
  "config": {"model": "...", "effort": "high", "options": {...}},
  "context"?: ContextUsage,
  "compaction": {"autoAtTokens": null, "autoAtPercent": null, "timeoutMs": 120000}}
```

`SessionInfo.config` is the current selection for future turns, including
native options. Empty/default portable fields and an empty options map may
be omitted from the object. The top-level `model` mirrors the default model
selection. These defaults may differ from an active turn's accepted
configuration, which is recorded on `turn.started`.

Session states:

```text
starting ──► idle ◄──► running
             idle ◄──► compacting
             idle ◄──► suspended  (restore restarts the runtime)
idle | running | compacting ──► closing ──► closed
any ──► failed            (the runtime died; see session.closed)
```

### `session.get`, `session.list`

`{"sessionId"}` → `{"session": SessionInfo}`, and `{}` → `{"sessions": [SessionInfo]}`.
Both read the session store. By default the sidecar keeps sessions in
memory, so they last only as long as the process. Start it with
`--store DIR` to persist session records (`<id>.json`) and ordered event
transcripts (`<id>.events.jsonl`) across restarts, or `--store default` to
share the `harness-kit` CLI's history. Stored sessions without a runtime in
this manager report `closed` (or `failed`) with no active turn, including
sessions interrupted by a process crash. Resume them with `session.restore`.

### `session.context`, `session.compact`

Both accept `{"sessionId"}` and return the same context snapshot:

```json
{
  "sessionId": "ses_…",
  "state": "idle",
  "canCompact": true,
  "usage": { "usedTokens": 48000, "windowTokens": 200000, "basis": "last_request", "stale": false }
}
```

`session.context` is a cached read: it does not start a runtime or issue a model
request. Each usage count is required but nullable. `null` means unknown; zero
means measured zero. `basis` is `reported` for native context occupancy,
`last_request` for the latest relevant model request's token count, or `unknown`.
`stale` marks an observation that should not be presented as current occupancy.
The same usage object appears in `SessionInfo.context`, `session.updated`, and
`context.usage` events. Cumulative usage never substitutes for context occupancy.

`session.compact` invokes native compaction and responds only after completion.
It requires an idle live session with `canCompact: true`; restore inactive
sessions explicitly first. Runtime capabilities `contextUsage` and `compaction`
describe potential support, while `canCompact` reflects current per-session
availability. Unsupported operations return `unsupported` without submitting
an ordinary prompt. The operation can make model requests and incur usage.

Await the preceding turn, then await compaction before prompting again. During
manual compaction, state is `compacting`; competing prompts/compactions return
`session_busy`. `session.cancel`, `session.close`, and `$/cancelRequest` can
interrupt it. The session remains reserved until native work stops. No saved
transcript events are deleted. See [context usage and compaction](context.md).

### `session.restore`

`{"sessionId"}` → `{"session": SessionInfo}`. Restarts a stored session
under the same ID, with its saved configuration, by resuming its
runtime-native session. Restoring a live session returns it unchanged. A
session that never ran a turn fails with `-32013 not_resumable`. Event
sequence numbers continue after the saved history and the record's last
sequence number; they never restart on restore.

### `session.rename`

`{"sessionId", "title"}` → `{"session": SessionInfo}`. Sets the session's
title. Titles are one line: the server turns runs of whitespace, including
newlines, into single spaces. An empty title, or one longer than 200
characters, fails with `-32602 invalid_params`. Any stored session can be
renamed, whether or not it is live. A title set this way, or at
`session.create`, is never replaced by the automatic one. Renaming does not
change `updatedAt`, which tracks activity. A `session.updated` event carries
the new title and mutable session state to every client connected to the
same manager, even when the renamed session is dormant. Independent
sidecar processes do not watch each other's files.

### `session.configure`

```json
{"sessionId": "ses_…", "config": {"model": "model-name", "effort": "high"}}
→ {"session": SessionInfo}
```

Updates defaults for subsequent turns and emits `session.updated` with the
new `config`. A running turn keeps its accepted snapshot. Stored sessions
can be configured without launching a runtime, provided their adapter is
registered. Creation/closing in progress fails with `session_busy`.
Configuration is checked against adapter capabilities, adapter validation,
and host constraints; failure leaves the defaults unchanged. Native
resolution that requires a live runtime is checked when a turn is accepted.

The `config` object is a patch:

```json
ConfigPatch = {"model"?: "...", "effort"?: "...", "options"?: {...},
               "reset"?: ["model", "effort", "options.maxTurns"]}
```

- Omitted fields are unchanged; `{}` is a no-op patch.
- `model: ""` or `effort: ""` explicitly requests native default selection.
  If the adapter cannot reliably resolve it without reusing an earlier
  override, it returns `unsupported`.
- `reset` removes this layer's selection. In `session.configure`, reset
  inherits the original creation settings. In `session.prompt`, reset
  inherits the current session defaults.
- Each supplied native option replaces that option's entire value. Options
  not mentioned are retained. `false`, `0`, `[]`, empty strings and native
  `null` values are preserved and remain subject to adapter validation;
  nested objects are not recursively merged.
- `config` must be an object. `model`, `effort`, `options` and `reset` cannot
  themselves be `null`; use `reset` to inherit. Unknown patch fields,
  duplicate reset entries, or setting and resetting the same field are
  `invalid_params` errors.

A setting that supports only startup cannot be changed here or for one
turn. Unsupported operations return `-32012 unsupported`; malformed
patches or invalid values return `-32602 invalid_params`. The host's
permission constraints apply to updates and turn overrides just as they do
to session creation; disabling guarded runtime protections without host
opt-in returns `-32009 policy_violation`.

### `session.delete`

`{"sessionId"}` → `{}`. Closes the session if it is live and removes its
record and event transcript from the store. The runtime's own transcript
is never deleted.

### `session.prompt`

```json
{"sessionId": "ses_…", "turnId"?: "client-uuid", "input": [{"type": "text", "text": "…"}],
 "config"?: {"model": "model-name", "effort": "high"}}
→ {"turnId": "client-uuid"}
```

The server responds as soon as the turn has started; the turn itself is
followed through events. **Clients should generate `turnId`**, for example
with a UUID. Events for the turn can arrive before the response, and a
client-chosen ID lets the client match them. A `turnId` already used in
the session fails with `-32602 duplicate_turn`.

`config` uses the patch rules above and applies to this turn only. Omitting
it uses the current session defaults; `config: null` is invalid. The manager
validates and snapshots the effective settings before accepting the turn.
Its `turn.started.data.config` records that snapshot, including any native
selection the adapter resolves. Later session changes cannot mutate it,
and this turn's overrides never become subsequent defaults. Rejected
configuration produces no `turn.started` event.

Only one turn or compaction runs per session. Prompting a running or compacting session fails with
`-32005 session_busy`. `image` input parts take an absolute `path`; not
every runtime supports them.

### `session.cancel`

`{"sessionId"}` → `{}`. Interrupts the active turn or manual compaction. The call is idempotent
and returns immediately; an interrupted turn then ends with `turn.completed`
`{"status": "cancelled"}`. Pending approvals for the turn are resolved with
source `cancelled`. Runtimes get a graceful interrupt first, then their
process tree is killed after a grace period.
Manual compaction ends with `context.compaction` status `cancelled`; it does not
create a user turn or `turn.completed` event.

### `session.close`

`{"sessionId"}` → `{}`. Cancels any active turn or compaction, releases the runtime and
emits `session.closed`. Closing a closed session succeeds.

### `session.events`, `session.eventsPage`

```json
{"sessionId", "afterSeq"?: 0, "limit"?: 0}
→ {"events": [Event], "truncated": false, "nextSeq": 42, "hasMore": true}
```

Returns events with `seq > afterSeq`, including saved sessions that have not
been restored. Use it for transcript replay and gap recovery without starting
a runtime. Responses are bounded by `history.maxPageEvents` and
`history.maxPageBytes`. `limit <= 0` uses the configured page size; a positive
limit can request fewer events. While `hasMore` is true, pass `nextSeq` as the
next `afterSeq`. A single event exceeding the byte limit returns
`event_too_large` (-32017) without advancing the cursor. Go and TypeScript
`Events` / `events()` compatibility helpers fetch pages and collect the result;
use `EventsPage` / `eventsPage()` to bound the caller's memory too.

With a `FileStore`, the complete JSONL history survives process restarts and
in-memory eviction. Without an `EventStore`, only the last `eventLogSize`
events of cached sessions remain available. `truncated: true` indicates
missing events after the requested cursor, such as a legacy record with no
transcript, eviction from a memory-only log, or failed writes. Reaching a
page limit alone does not mean truncation.

A partially written final JSONL line is ignored and removed on the next
append. Corrupt complete lines produce an error rather than silently losing
history. An append failure is logged and emits an `error` event with code
`event_store_error`; the live in-memory tail remains available for replay.
One active writer per session is required across processes/store instances.

### `session.evict`, `session.suspend`

`{"sessionId": "ses_…"}` → `{}`. Eviction releases an idle session's cached
transcript; suspension also releases its runtime. Both require a durable event
store and protect pinned sessions, active turns/compactions and unsaved events. Suspension
preserves the conversation ID and produces `suspending` then `suspended` state
updates. `session.restore` resumes it; `memory.resumeOnPrompt` enables automatic
resume when the next prompt arrives. Closing a suspended session disables that
automatic resume. Set pinning through the same settings API:

```json
settings.configure {"target":{"sessionId":"ses_…"},"patch":{"memory":{"pinned":true}}}
```

### `approval.respond`

```json
{"approvalId": "apr_…", "outcome": "allow" | "allow_session" | "deny" | "abort",
 "message"?: "shown to the agent", "updatedInput"?: {...}}
→ {}
```

The outcomes:

- `allow_session`: allow this and similar future requests in the session,
  where the runtime supports it.
- `deny`: refuse this one action; the agent continues.
- `abort`: refuse and stop the turn.

An outcome not listed in the request's `outcomes` fails with `-32010
invalid_decision`. An unknown or already-resolved ID fails with `-32007
approval_not_found`.

### `tools.register`, `tools.unregister`, `tools.list`

Clients can register **host tools**, whose handlers run in the client
([tools.md](tools.md)):

```json
tools.register {"tools": [{"name": "lookup", "description": "...", "inputSchema": {...},
                           "approval": "auto"|"ask", "readOnly": false, "timeoutMs": 30000}]} → {}
tools.unregister {"names": ["lookup"]} → {}
tools.list {} → {"tools": [ToolSpec]}
```

Registration is all or nothing. A name already taken fails with `-32602
duplicate_tool`. A client can unregister only its own tools, and they are
removed when it disconnects. Register tools before creating the sessions
that should use them. `session.create` accepts `hostTools` (limit to some
tools) and `disableHostTools`.

When a runtime calls one, the **server sends a request to the client**:

```json
← {"jsonrpc":"2.0","id":7,"method":"tool.call","params":{"sessionId":"ses_…","turnId":"…","name":"lookup","arguments":{...}}}
→ {"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"..."}],"isError":false}}
```

Arguments have already been validated against `inputSchema`, and
`approval: "ask"` tools have already been approved. If the call is
abandoned, for example because the turn was cancelled, the server sends
`$/cancelRequest {"id": 7}`. Only clients that register tools receive
`tool.call`. `capabilities.tools` in `initialize` reports whether the
server hosts tools.

### `mcp.list`

```json
{"cwd"?: "/abs/project", "runtimes"?: ["codex", "claude"]}
→ {"enabled": true, "servers": [{"name": "github", "source": "user", "loaded": true, "server": {...}}],
   "inheritNative": true, "userFile": "…", "projectFile": "…", "projectTrusted": false,
   "native": [{"runtime": "codex", "supported": true, "servers": ["Canva", "…"]},
              {"runtime": "claude", "supported": false, "servers": []}]}
```

This describes the kit MCP configuration seen from `cwd`, for a settings
screen ([mcp.md](mcp.md)). Servers from an untrusted project file appear
with `loaded: false`. `enabled` is false unless the sidecar runs with
`--kit-mcp`. `native` lists each requested runtime's own servers where the
runtime can report them (Codex, via `config/read`; Claude Code cannot).
`${VAR}` references are returned as written, never expanded.

### `approval.list`

`{"sessionId"?}` → `{"approvals": [ApprovalRequest]}`. These are the
pending requests, useful for rebuilding a UI.

### Request cancellation

`$/cancelRequest` `{"id": <request id>}` (a notification) cancels an
in-flight request such as a slow `runtime.discover` or `session.compact`. The
request fails with `-32800`; compaction cancellation also interrupts the native
operation. Cancelling a `session.prompt` request after it accepted a turn does
not interrupt that turn; use `session.cancel`, which can cancel either a turn
or manual compaction.

## Events

The server sends every session event as an `event` notification:

```json
{
  "jsonrpc": "2.0",
  "method": "event",
  "params": {
    "seq": 42,
    "sessionId": "ses_…",
    "turnId": "…",
    "type": "message.delta",
    "time": "2026-11-03T10:00:00.123Z",
    "data": { "itemId": "…", "text": "Hel" }
  }
}
```

| type                                     | data                                                                                                                                                                           |
| ---------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `session.started`                        | `runtime`, `runtimeSessionId`, `resumed`, `model`                                                                                                                              |
| `session.updated`                        | `title`, `state`, `turns`, `updatedAt`, `activeTurnId?`, `runtimeSessionId?`, `lastStatus?`, `error?`, `config?` (current defaults), `context?`, `compaction?`                 |
| `session.closed`                         | `reason` (`closed`, `failed`, `shutdown`), `error?`                                                                                                                            |
| `turn.started`                           | `text`, `input?` (ordered text and local image references), `config?` (accepted effective configuration)                                                                       |
| `turn.completed`                         | `status` (`completed`, `cancelled`, `failed`), `error?`, `usage?`, `runtimeTurnId?`                                                                                            |
| `message.delta`                          | `itemId`, `text`, `parentCallId?` (subagents)                                                                                                                                  |
| `message.completed`                      | `itemId`, `role`, `text`, `parentCallId?`                                                                                                                                      |
| `reasoning.delta`, `reasoning.completed` | `itemId`, `text`                                                                                                                                                               |
| `tool.started`                           | `callId`, `name`, `kind` (`command`, `file_change`, `file_read`, `search`, `web_search`, `mcp`, `subagent`, `other`), `title?`, `input?`, `parentCallId?`                      |
| `tool.output`                            | `callId`, `output` (incremental)                                                                                                                                               |
| `tool.completed`                         | `callId`, `name?`, `status` (`completed`, `failed`, `declined`), `output?`, `exitCode?`, `error?`                                                                              |
| `approval.requested`                     | ApprovalRequest: `approvalId`, `kind`, `title`, `command?`, `cwd?`, `paths?`, `reason?`, `tool?`, `input?`, `outcomes?`                                                        |
| `approval.resolved`                      | `approvalId`, `decision`, `source` (`host`, `policy`, `timeout`, `cancelled`), `request?` (included when a policy decided without asking)                                      |
| `usage`                                  | `scope` (`turn`, `session`), token counts, `contextWindow?`, `costUsd?` (the runtime's estimate)                                                                               |
| `context.usage`                          | `usedTokens` and `windowTokens` (required nullable counts), `basis` (`reported`, `last_request`, `unknown`), `stale`                                                           |
| `context.compaction`                     | `operationId`, `reason` (`manual`, `threshold`, `native`), `status` (`started`, `completed`, `failed`, `cancelled`), `before`, `after?` (context usage observations), `error?` |
| `runtime.status`                         | `status` (`ready`, `retrying`, `rate_limited`, `warning`, `permission_denied`, `compacted`, …), `message?`, `detail?`                                                          |
| `error`                                  | `code`, `message`, `retryable?` (non-fatal; fatal errors end the turn)                                                                                                         |
| `runtime.raw`                            | `runtime`, `kind`, `payload` (only with `rawEvents: true`)                                                                                                                     |
| `stream.gap`                             | `fromSeq`, `toSeq` (delivery marker, see below)                                                                                                                                |

`session.updated` contains a full snapshot of the mutable fields above:
an absent optional field has been cleared. It is emitted on startup,
rename, configuration changes, turn/compaction start and completion, and closing. Its
envelope supplies the session ID and sequence number. Clients can update title and state without
fetching the record again. Turn lifecycle updates carry that turn's ID;
independent rename/configure updates are session events. Historical events
written before configuration snapshots were introduced may omit `config`.

### Ordering guarantees

- Within a new session, `seq` starts at 1 and increases by exactly 1 per
  event. Restoring continues the sequence. Events are delivered in `seq`
  order; missing historical events are reported as truncation.
- Every `turn.started` is followed by exactly one `turn.completed` with the
  same `turnId`. Turns of one session never interleave: the next turn's
  `turn.started` comes after the previous `turn.completed`. A client may
  send the next `session.prompt` as soon as it sees `turn.completed`.
- An `approval.requested` is always followed by an `approval.resolved` with
  the same `approvalId`, before the turn completes.
- There is no ordering between different sessions.
- `session.prompt`, `session.cancel`, `session.close`, `session.restore`,
  `session.rename`, `session.configure`, `session.delete` and `git.*` requests for the
  same session take effect in the order the client sent them. Other
  requests run concurrently, so a slow `runtime.discover` never delays an
  `approval.respond`. A `session.configure` sent before `session.prompt`
  takes effect for that prompt, even if the client pipelines both requests.
  A configure request after prompt acceptance affects later turns.
- `session.compact` runs outside that request queue so cancellation and closing
  remain available while it awaits the runtime. The manager reserves the session
  synchronously at admission. Callers must await the preceding turn and the
  compaction response; pipelining these operations does not establish their order.
  A kit threshold compaction runs within its accepted turn, before sending the
  prompt to the runtime, and emits events with that turn's ID. Manual compaction
  has no turn ID. Context events use the same ordered stream and replay rules.

### Backpressure and gap recovery

The server never blocks a runtime because a client reads slowly. Each
client connection has a bounded event queue (`--event-buffer`, default
4096):

1. When the queue is full, the client enters a _lagging_ state. Further
   events are dropped for it, and only for it; they remain in the session's
   event log.
2. When the queue has drained, the client receives one `stream.gap` event
   per affected session, `{"fromSeq": a, "toSeq": b}`, then normal delivery
   resumes with `seq > b`.
3. The client fetches `a..b` with `session.events({afterSeq: a - 1})`.

The TypeScript client's high-level `Turn` does this automatically. Go clients
using event callbacks handle the marker explicitly: request bounded pages with
`SessionEventsPage`, advance `afterSeq` to `nextSeq` while `hasMore` is true, and
deduplicate by session ID and sequence. A truncated page means some history is
unavailable. A `stream.gap` event is never stored, so it never appears in
`session.events`.

## Errors

Every Harness Kit error has `error.data.reason`, a stable string. Switch on
it rather than on the message.

| code   | reason                             | meaning                                                                               |
| ------ | ---------------------------------- | ------------------------------------------------------------------------------------- |
| -32700 |                                    | parse error (bad JSON, oversized line)                                                |
| -32600 |                                    | invalid request (batch, double initialize)                                            |
| -32601 |                                    | method not found                                                                      |
| -32602 | `invalid_params`, `duplicate_turn` | malformed params, invalid configuration values, or reused turn ID                     |
| -32002 | `not_initialized`                  | `initialize` first                                                                    |
| -32800 |                                    | request cancelled via `$/cancelRequest`                                               |
| -32001 | `session_not_found`                |                                                                                       |
| -32003 | `runtime_not_found`                | not registered with this sidecar                                                      |
| -32004 | `runtime_unavailable`              | not installed                                                                         |
| -32005 | `session_busy`                     | a turn, compaction, or conflicting Git operation is running                           |
| -32006 | `session_closed`                   |                                                                                       |
| -32007 | `approval_not_found`               | unknown or already resolved                                                           |
| -32008 | `unsupported_version`              | see `data.supportedVersions`                                                          |
| -32009 | `policy_violation`                 | unsafe options without `--allow-unsafe`, or writes/remotes disabled by Git policy     |
| -32010 | `invalid_decision`                 | outcome not supported by the request                                                  |
| -32011 | `runtime_error`                    | the runtime failed (start, bad options, …)                                            |
| -32012 | `unsupported`                      | runtime does not support the operation, setting lifetime, or native-default selection |
| -32013 | `not_resumable`                    | the stored session never started, so there is nothing to resume                       |
| -32020 | `git_error`                        | native Git operation failed                                                           |
| -32021 | `git_conflict`                     | operation conflicts with repository state                                             |

## Example session

```text
→ {"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientInfo":{"name":"demo"}}}
← {"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"serverInfo":{"name":"harness-kit","version":"dev"},"capabilities":{…}}}
→ {"jsonrpc":"2.0","id":2,"method":"session.create","params":{"runtime":"fake"}}
← {"jsonrpc":"2.0","id":2,"result":{"session":{"sessionId":"ses_1","state":"idle",…}}}
← {"jsonrpc":"2.0","method":"event","params":{"seq":1,"sessionId":"ses_1","type":"session.started",…}}
← seq 2 session.updated (idle)
→ {"jsonrpc":"2.0","id":3,"method":"session.prompt","params":{"sessionId":"ses_1","turnId":"t1","input":[{"type":"text","text":"[approve] hi"}]}}
← {"jsonrpc":"2.0","id":3,"result":{"turnId":"t1"}}
← {"jsonrpc":"2.0","method":"event","params":{"seq":3,"turnId":"t1","type":"turn.started",…}}
← seq 4 session.updated (running, activeTurnId t1)
← {"jsonrpc":"2.0","method":"event","params":{"seq":5,"turnId":"t1","type":"approval.requested","data":{"approvalId":"apr_1","kind":"command","title":"Run `echo hello`",…}}}
→ {"jsonrpc":"2.0","id":4,"method":"approval.respond","params":{"approvalId":"apr_1","outcome":"allow"}}
← {"jsonrpc":"2.0","id":4,"result":{}}
← seq 6 approval.resolved · 7 tool.started · 8 tool.output · 9 tool.completed · 10–11 message.delta · 12 message.completed · 13 usage
← seq 14 session.updated (idle, no activeTurnId)
← {"jsonrpc":"2.0","method":"event","params":{"seq":15,"turnId":"t1","type":"turn.completed","data":{"status":"completed",…}}}
```

Responses and event notifications are written independently, so their
relative order can vary from this transcript (an event may precede the
response to the request that caused it). Per-session event order never
varies. Try it by hand with `harness-kit --runtimes "" --fake-runtime`.

### Catalog filters and forks

`session.list` accepts optional `query`, `searchTranscript`, `tags`, `archived`,
`runtime`, `cwd` and `limit`. Omitted `archived` includes both archived and
unarchived records. Results are newest activity first. Negative limits and
queries longer than 1024 characters are rejected.

`session.archive {sessionId, archived}` and `session.tags {sessionId, tags}`
return `{session: SessionInfo}` and publish `session.updated`. Tags replace the
canonical JSON array in `metadata.tags`; `[]` clears tags. Archive state is
independent of lifecycle and does not cancel an active turn.

`session.fork {sessionId, title?, settings?}` returns `{session: SessionInfo}`.
It branches the entire completed conversation with durable transcript lineage.
`settings` accepts creation-time session selections (except runtime/resumeId)
and `memory.pinned`. New fields are `forkedFrom`, `forkedAtSeq`, `inheritedTurns`,
`forkPending`, and `archived`. Runtime capabilities advertise `fork` and
`forkDeferred`. Claude forks remain pending until the first actual prompt; an
advanced source fails with `-32018 fork_source_changed` instead of silently
branching newer native history. See [session semantics](sessions.md).

### Compaction settings

The shared `compaction` group accepts nullable positive `autoAtTokens`, nullable
`autoAtPercent` greater than zero and at most 100, and positive `timeoutMs`
(default 120000). Both thresholds default to `null` (disabled). Global settings
provide new-session defaults, session updates apply next operation, and turn
settings override that turn's policy. Either threshold can trigger before the
next prompt from the latest non-stale observation; unknown counts cannot trigger
it. A pre-turn compaction failure fails the accepted turn before its prompt is
submitted. These host thresholds do not disable native automatic compaction.
See [context usage and compaction](context.md) for runtime differences.

### Usage budgets

The shared `budget` settings group has nullable `maxSessionTokens`,
`maxSessionCostUsd`, `maxTurnTokens`, and `maxTurnCostUsd`. Global scope supplies
new-session defaults; session scope changes limits live; turn scope accepts only
the two turn limits. Null means unlimited and zero denies new work. See
[usage accounting and limitations](budgets.md).

`session.prompt` returns `-32019 budget_exceeded` when admission is exhausted.
During a turn, `budget.exceeded` carries `{scope, metric, limit, observed}` and
requests cancellation. The same object appears as optional `budget` in the final
`turn.completed` result. Usage observed after inference does not turn already
completed work into cancelled work. Session snapshots include `budget` policy and
`usage` totals with `totalTokens`, optional estimated `costUsd`, `tokensComplete`
and `costComplete`.

`usage.mode` defaults to `snapshot`; `delta` explicitly means an increment.
`usage.tokensKnown` can distinguish a measured zero from unknown usage. Native
context occupancy and context-window sizes never count toward token budgets.

### Workspaces and turn diffs

The shared workspace settings group controls opt-in worktree creation and turn
capture. Global values are defaults for new sessions; scoped capture limits
apply next turn, and location changes require a new session. Initialization
advertises capabilities.workspace when the host supplies a native backend.

session.diff accepts {sessionId, turnId, maxBytes?} and returns the bounded saved
workingTree and index patches, before/after snapshot metadata, complete, error,
truncated and encodingLossy. These are differences between turn boundaries,
including nonignored untracked files and concurrent external edits. They are
not a cumulative diff against HEAD or proof of agent authorship.

workspace.diff events carry the same saved result before turn.completed;
turn.started.workspace records the accepted capture policy. Missing captures
return reason turn_diff_not_found. Saving patches in the transcript allows
retrieval without a live runtime, workspace or Git installation.

workspace.remove accepts {sessionId}. It requires a closed/suspended runtime,
verified ownership and a clean worktree, including ignored files, and refuses
advanced detached HEADs. Session deletion never removes workspace files.
See [workspace settings, boundaries and cleanup](workspaces.md).

### Source control

Initialization advertises `capabilities.git` when the workspace adapter implements
the optional source-control interface. Every operation targets an existing
`sessionId`; arbitrary repository paths are not accepted as RPC parameters.
Closed and suspended sessions can use these methods without starting a runtime.

| Method                              | Additional params                                | Result                                                  |
| ----------------------------------- | ------------------------------------------------ | ------------------------------------------------------- |
| `git.status`                        | none                                             | root, HEAD/branch, upstream, files, remotes, truncation |
| `git.diff`                          | `path?`, `staged?`, `maxBytes?`                  | patch, truncation, encoding loss                        |
| `git.branches`                      | none                                             | branches, truncation                                    |
| `git.log`                           | `ref?`, `limit?`                                 | commits, truncation                                     |
| `git.stage`, `git.unstage`          | `paths`                                          | `{}`                                                    |
| `git.commit`                        | `message`                                        | commit ID                                               |
| `git.checkout`                      | `branch`, `create?`, `base?`                     | `{}`                                                    |
| `git.fetch`, `git.pull`, `git.push` | `remote?`, `branch?`, `setUpstream?` (push only) | bounded output, truncation                              |

Global `git` settings in the unified settings document take effect at the next
operation. Reads and writes honor cancellation and deadlines. Writes coordinate
with agent turns in the same checkout unless the host opts into concurrency.
The native backend serializes operations that share a repository. Responses are
request results, not durable transcript events; hosts refresh after writes and
choose their own polling or filesystem notification strategy for external edits.

See [Git integration](git.md) for settings, literal-path behavior, bounded output,
credential handling and optional embedded adapters. Exact request/result fields
are generated in the [protocol schema](../../protocol/schema.json).
