# Dependencies and replaceable components

Harness Kit accepts collaborators at the boundaries where an application may
need different behavior. The application assembles runtimes, persistence, tool
execution, approvals, workspaces, model providers, and transports. Settings are
plain values; they do not locate services or create dependencies on the caller's
behalf.

Interfaces belong to the code that consumes the dependency. Optional capabilities
extend a small required contract. A replacement store need not implement Git,
and a static tool catalog need not implement change notifications.

## Core boundaries

| Area                     | Replacement boundary                                                                     | Default or supplied implementation                                              |
| ------------------------ | ---------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------- |
| Delegated agents         | `agent.Runtime`, `agent.Session`, and optional capabilities such as `agent.Compactor`    | Codex, Claude Code, ACP, or a host-defined runtime                              |
| Native process execution | `runtimes/transport.Launcher` and `Child`                                                | Internal OS launcher, or injected discovery and child I/O/lifecycle             |
| Image attachments        | `agent.InputFileOpener` returning `fs.File`                                              | Local files, or host-supplied files for Claude Code and ACP                     |
| Model inference          | `model.Provider`                                                                         | Separate OpenAI and Anthropic provider modules, or a host-defined provider      |
| Model context policy     | `model/contextwindow.Estimator` and `Compactor` callbacks                                | Host tokenizer and summary strategy; no model or tokenizer is chosen implicitly |
| Session records          | `session.Store`                                                                          | In-memory records or native `FileStore`                                         |
| Transcript persistence   | Optional `EventStore`, `EventPageStore`, `EventScanner`, `TurnLookup`, and `EventCopier` | Native JSONL implementation or an application database/object store             |
| Workspaces               | `workspace.Backend`, optional `SourceControl` and `SnapshotReleaser`                     | Native Git adapter or a host implementation                                     |
| Tool execution           | `tools.Handler`; `toolhost.ToolRegistry` for catalog and execution                       | `tools.Registry` or a custom local/remote tool backend                          |
| Tool catalog changes     | Optional `toolhost.ToolChangeNotifier`                                                   | Subscribe through `OnChange`; static catalogs omit it                           |
| Agent-facing tool bridge | `session.ToolServer`                                                                     | Native MCP `toolhost.Host` or a replacement bridge                              |
| Approval decisions       | `permission.Policy` and the session approval broker                                      | Host policy and user decisions                                                  |
| Configuration sources    | `session.Options.Prepare`, `PrepareRestore`, and `ValidateConfig`                        | Explicit startup values, application configuration, or opt-in file resolution   |
| Diagnostics              | Supplied `slog.Logger` and session logging options                                       | Host handlers and the kit's bounded structured log stream                       |

For example, an embedding application can assemble the portable session manager
without selecting any native implementation:

```go
func NewManager(
    runtime agent.Runtime,
    records session.Store,
    workspaces workspace.Backend,
    bridge session.ToolServer,
) *session.Manager {
    return session.NewManager(agent.NewRegistry(runtime), session.Options{
        Store: records,
        Workspace: workspaces,
        ToolServer: bridge,
    })
}
```

The store extensions are capabilities, not a requirement to implement every
method. A record-only store retains the manager's bounded in-memory transcript.
Durable replay and suspension require transcript storage. Applications replacing
a component must preserve its documented cancellation, ordering, ownership, and
durability guarantees.

## Tools and MCP

`toolhost.Options.Registry` consumes `Get`, `List`, and `Call`. A custom backend
does not need to embed `tools.Registry` or expose registration methods. The host
still enforces the session's tool allowlist and approval metadata; the injected
backend owns argument validation, execution limits, and cancellation. Catalogs
that change dynamically can additionally implement `OnChange(func()) func()`.
The returned function unsubscribes the listener when the host closes.

MCP configuration values and merging are independent of storage. `mcp.Resolver`
is the optional file-backed implementation for user/project configuration. Its
`Trust` field accepts `mcp.TrustChecker`, which decides whether the exact path and
file bytes are trusted. Applications can use the native `TrustStore` or inject an
account/database policy; a missing checker denies project servers. Applications
can also supply resolved session MCP configuration through
the preparation hooks instead of loading those files. RPC consumers accept an MCP
overview provider, so an application-owned configuration source can also supply
the settings interface's inventory.

The MCP HTTP client accepts an `http.Client`, whose transport can provide custom
authentication, proxies, tracing, or deterministic test responses. MCP stdio
accepts `client.Options.Launcher`, the same `transport.Launcher` process boundary
used by native runtime adapters. An injected launcher owns executable discovery,
environment inheritance, process startup, and child termination; the default
launcher uses local OS processes. Scripted child implementations can exercise the
complete MCP handshake and tool calls without launching a process.

The Unix-socket tool host remains a native bridge implementation; applications
can replace `session.ToolServer` for other transports.

## Model providers and native adapters

Codex, Claude Code, and ACP accept `WithLauncher` for all executable discovery,
session processes, and version/authentication probes. The default launcher stays
inside `internal/process` and preserves process-group interruption/termination.
The public transport package contains interfaces and command descriptions only.
An injected launcher returns a `Child` with stdin/stdout pipes, `Wait`,
`Interrupt`, and `Kill`; it does not need to return an `exec.Cmd` or start an OS
process. This lets tests exercise real protocol parsing with in-memory children.

The startup context bounds admission only. A successfully returned child must
outlive it; adapters explicitly stop children on cancellation or close. `Wait`
must leave unread stdout available until its caller drains or closes the pipe.
`Kill` must terminate descendants and unblock pending I/O. Command environment
entries overlay the launcher's inherited environment. These contracts also apply
to the MCP stdio client.

Claude Code and ACP accept `WithInputFiles`. The opener receives an image path
resolved against the session directory and returns an `fs.File`; the adapter
closes it and still validates file type, size, and media format. Paths may be
absolute, so map them explicitly when supplying a rooted virtual filesystem.
ACP's separate `WithFileSystem` callbacks serve agent-requested text operations.

Discovery deadlines belong to each registry instead of mutable package state:

```go
func NativeAgents(
    launcher transport.Launcher,
    images agent.InputFileOpener,
) *agent.Registry {
    return agent.NewRegistryWithOptions(agent.RegistryOptions{
        DiscoverTimeout: 10 * time.Second,
    },
        codex.New(codex.WithLauncher(launcher)),
        claude.New(claude.WithLauncher(launcher), claude.WithInputFiles(images)),
        acp.New("agent-command", acp.WithLauncher(launcher), acp.WithInputFiles(images)),
    )
}
```

`agent.NewRegistry` keeps the 20-second discovery default. Nil optional launcher
or input-file dependencies retain the native defaults. The sidecar command is
the composition root: it intentionally selects concrete native implementations,
while embedding applications assemble their own dependencies.

The OpenAI and Anthropic providers implement the same `model.Provider` contract.
Each accepts the official SDK's request options and a supplied SDK client through
`NewWithClient`. HTTP transport and credentials remain configurable through those
SDK options. A second interface around every SDK type would add no replacement
capability: applications can inject HTTP behavior or replace the provider itself.

Likewise, `workspace/git` deliberately runs Git and accesses the local filesystem,
and `session.FileStore` deliberately uses local files, atomic replacement, and
synced JSONL writes. Their portable consumers depend on workspace/store
interfaces. Replacing native filesystem calls with a generic filesystem interface
would need to preserve those stronger semantics; an `io/fs.FS` alone cannot do
that.

## Audit scope and verification

The boundary review covers runtime adapters, subprocess discovery/execution,
image inputs, registry deadlines, the model contract and both provider adapters, caller
context management, tools and the MCP bridge, MCP configuration/client behavior,
workspace interfaces/native Git, and session storage extensions. It checks that
consumers use replaceable collaborators and that supplied dependencies are not
silently replaced by built-in implementations.

This is not a claim that every clock, random identifier, filesystem call, or
internal helper has its own interface. Native adapter integration tests use
temporary repositories/files and scripted subprocesses where OS behavior is the
subject of the test. Protocol behavior is also tested with injected in-memory
children and attachment files. Portable behavior can be tested with fake runtimes,
custom stores, custom tool registries, and injected provider HTTP responses,
without credentials or paid model requests.

See [Architecture](architecture.md), [Configuration](configuration.md),
[Tools](tools.md), [MCP](mcp.md), and [Models and context](models.md) for the
individual contracts and examples.
