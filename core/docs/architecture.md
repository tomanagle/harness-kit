# Architecture

Harness Kit provides composable Go packages and a local sidecar for building
agent harnesses. Go applications embed the packages; applications in other
languages use the generated clients. Both reach the same session manager.
The core module has no third-party dependencies.

The [package boundary guide](packages.md) records the layout, public API inventory,
research sources and compatibility policy. The [dependency guide](dependencies.md)
describes what applications can replace and the contracts replacements must meet.

## Repository and module boundaries

- `core/`: the Go library, adapters, local CLI/sidecar and documentation.
- `core/providers/`: separate OpenAI and Anthropic modules using official SDKs.
- `packages/`: independent Go and TypeScript sidecar clients.
- `protocol/`: generated wire schema and the generation guide.
- `examples/`: consumer applications, including independent Go modules.
- `server/`: placeholder for future cross-device coordination.

The local sidecar works independently of the future coordination server.
Provider SDKs and desktop frameworks do not enter the core dependency graph.

## Composition and dependency direction

```text
Electron / Node / other clients       Go embedding applications
             |                                  |
    generated client                            |
             | JSON-RPC                         |
    cmd/harness-kit                             |
             |                                  |
       internal/cli <-- chooses native adapters  |
             |                                  |
         rpc/server                             |
             |                                  |
     internal/rpcserver                         |
             +---------------+------------------+
                             |
                      session.Manager
                             |
       agent / settings / hooks / event / permission / workspace
                             ^
                             | implements injected contracts
       runtime adapters / stores / workspace backend / application code
```

`cmd/harness-kit` owns the executable entry point, release version and process
exit. `internal/cli` chooses concrete runtimes, native stores, Git, MCP and the
stdio host. An embedded application makes those choices itself.

`rpc/server` is the supported hosting API. Its private implementation handles
handshake, dispatch, connection-owned hooks/tools, bounded subscriptions, error
translation and request ordering. `internal/jsonrpc` supplies framing and
request cancellation. Neither transport package defines session behavior.

`session.Manager` owns session and turn lifecycle, admission, persistence,
approvals, budgets, context observations and resource policy. It depends on
runtime and workspace contracts, not Codex, Claude, ACP, Git or JSON-RPC.
The private `managed` state and its synchronization stay in the same package.
Files separate responsibilities; see the [session file map](packages.md#why-session-internals-stay-in-the-session-package).

## Two execution tracks

Delegated sessions use `agent.Runtime` and `agent.Session`. An adapter drives an
existing agent, translates its output into normalized events, and reports
capabilities and unsupported operations. Native conversation state remains
owned by that runtime; kit transcripts are persisted observations, not a
replacement for its resume state.

A custom agent loop uses `model.Provider`, `tools`, `permission`, and
`model/contextwindow`. The application owns the loop, tool execution, tokenizer,
compaction strategy and history storage. Model providers do not implicitly run
tools or choose a context policy. This is why context-window management is a
public building block while usage-accounting reconciliation is internal.

See [Runtime adapters](runtimes.md), [ACP](acp.md), and [Models and context](models.md).

## State, settings and extensions

`session.Store` persists records. Optional store capabilities add transcript
paging, scanning, turn lookup and copying. The supplied file store uses JSON
records and JSONL events with a single writer per session. Runtime-native IDs,
accepted-turn counts, accounting checkpoints and configuration survive restore.

`settings.Values` is the startup/runtime settings document. The manager validates
updates and applies their declared lifetimes. Optional `settings.Store`
persistence saves accepted global changes before applying them; process wiring
is selected at startup. Session configuration and per-turn overrides use the
same configuration semantics. See [Configuration](configuration.md).

`hooks` provides creation and turn lifecycle extension points. Registrations have
owners, deterministic order, cancellation and concurrency limits. Remote
registrations belong to their RPC connection. After-turn hooks are best-effort
observations, not durable jobs. See [Lifecycle hooks](hooks.md).

Application collaborators are injected through runtime, store, tool, permission,
workspace, hook and transport boundaries. Interface declarations stay with their
consumers or public extension contracts. Native implementations can use local
filesystem/process facilities without forcing those dependencies into their
portable consumers.

## Events, logs and transport

Events carry normalized payloads and per-session sequence numbers. Bounded
subscriber queues protect runtime producers from slow consumers; gap markers
make loss visible and history paging supports recovery. The client helpers own
turn correlation, replay and conversation projection. See [Protocol](protocol.md)
and [Sessions](sessions.md).

Diagnostics use Go's `slog`. A bounded structured stream can be forwarded to a UI
and combined with application handlers. Logs and conversation events have
separate purposes and subscriptions. See [Logging](logging.md).

Host tools reach delegated agents through MCP. `tools` defines the tool inventory
and execution, `toolhost` supplies the native bridge, and `mcp/client` allows
explicit outbound MCP connections. Applications can replace these boundaries.
See [Tools](tools.md), [MCP](mcp.md) and [Security](security.md).

## Portability and generated clients

The portable packages and RPC host build for `wasip1/wasm` and `js/wasm` and must
not transitively import `os/exec` or the native process implementation. Native
adapters, the Git backend, the MCP client and tool bridge are explicit native
components. These build checks do not imply that a browser host shim is supplied.

`cmd/protocolgen` invokes `internal/protocolgen` to extract the Go wire definitions
and method registry into `protocol/schema.json`. Both clients are rendered from
that serialized schema; they do not depend on the generator or core at runtime.
CI checks generated output. See [Protocol generation](../../protocol/README.md).

`make architecture-check` enforces the public package inventory and dependency
direction. It also runs within the normal Go test suite. Independent example and
provider modules validate external use of public APIs; `make wasm-check` and
`make cross` validate platform builds.
