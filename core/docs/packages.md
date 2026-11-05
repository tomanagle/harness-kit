# Package boundaries

Harness Kit's core is a Go library with an accompanying command. Harness builders
can embed it in a Go application or use the sidecar through the generated clients.
That makes its public packages a product API, not just folders for the executable.

`internal` is an enforced import boundary. Packages below `core/internal` are
private to the `github.com/tomanagle/harness-kit/core` import tree. A sibling
application or provider module does not gain access just because it shares this
repository. Unexported identifiers already hide implementation inside a public
package; a separate package is useful when it owns a coherent responsibility.

## Layout

```text
core/
  cmd/
    harness-kit/main.go     executable, version metadata, process exit
    protocolgen/main.go     generator flags and process exit
  internal/
    cli/                   CLI commands and native sidecar composition
    protocolgen/           schema extraction and client rendering
    rpcserver/             protocol dispatch, connection state, subscriptions
    rpcclient/             private CLI client using core-domain types
    jsonrpc/               framing, cancellation, request ordering
    process/               native discovery and process-tree supervision
    budget/                usage reconciliation and accounting checkpoints
    processtest/           scripted process helpers, test imports only
    integration/           opt-in live cross-runtime tests
    architecture/          package/dependency boundary tests
  agent/                   runtime contracts and discovery
  session/                 public lifecycle API, stores, orchestration
  ...                      public building blocks listed below
  providers/               separate modules with official model SDK dependencies
  docs/                    architecture and feature guides
```

Repository-root `packages/` contains the standalone clients, `examples/` contains
consumer applications, and `protocol/` contains the generated wire schema.
`server/` remains reserved for the future coordination server. Local sidecar
composition belongs to this core, not that future service.

## Public API inventory

| Packages                                                             | Why a builder imports them                                                             |
| -------------------------------------------------------------------- | -------------------------------------------------------------------------------------- |
| `agent`                                                              | Implement or register a runtime; discover capabilities; supply input and configuration |
| `session`                                                            | Embed lifecycle management; supply stores, approval policy and workspace dependencies  |
| `event`                                                              | Consume normalized events, use bounded logs and subscriptions                          |
| `config`, `settings`                                                 | Compose configuration values; validate, persist and update the settings document       |
| `hooks`                                                              | Register or replace lifecycle execution through public handler/registry contracts      |
| `logging`                                                            | Attach the bounded `slog` stream to an application's own diagnostics                   |
| `permission`                                                         | Supply approval policies and resolve decisions                                         |
| `tools`, `toolhost`                                                  | Define tools and optionally expose them to agents over the native MCP bridge           |
| `mcp`, `mcp/client`                                                  | Configure MCP servers or connect to and invoke them directly                           |
| `model`, `model/contextwindow`                                       | Implement an inference loop with caller-owned history, estimation and compaction       |
| `workspace`, `workspace/git`                                         | Supply workspace behavior or select the native Git implementation                      |
| `rpc/protocol`, `rpc/server`                                         | Work with wire types or serve an embedded manager over an owned transport              |
| `runtimes/codex`, `runtimes/claude`, `runtimes/acp`, `runtimes/fake` | Explicitly select an adapter; use the fake in application tests                        |
| `runtimes/transport`                                                 | Inject executable discovery and child-process lifecycle into native adapters           |

The module root is a documentation package. There is no catch-all service
container or second public API that duplicates these packages.

### Why context windows stay public and budgets do not

`model/contextwindow` is used directly by `examples/model`, which is a separate
consumer module. Applications supply its estimator and compactor and decide how
to persist history. Moving it wholesale into `internal` would remove that
extension point. Its files separate history (`window.go`), fitting (`fit.go`),
and estimation (`estimate.go`) without creating artificial package dependencies.

Budget _policy_ and observable usage are public: `config.BudgetLimits` and
`event.BudgetUsage`. The accounting ledger and snapshot/delta reconciliation are
private in `internal/budget`. Custom stores preserve `Record.Accounting` as
opaque JSON. They do not need the ledger implementation.

The same distinction applies to process handling: `runtimes/transport` exposes
contracts builders can implement, while `internal/process` implements the local
OS behavior. RPC hosting keeps its public `Server`, `Options`, and dependency
interfaces; connection state and dispatch live in `internal/rpcserver`.

### Why session internals stay in the session package

The manager's state machine, locks, active turns and persistence operations form
one cohesive implementation. Moving them to multiple packages would require
exporting private state or introducing circular dependencies with `Info`,
`Record`, `Store` and the public manager. Files provide navigation without a new
API boundary:

- `manager.go`, `options.go`: manager ownership, construction and lifecycle.
- `create.go`, `restore.go`, `records.go`: admission, restoration and catalog access.
- `turn.go`, `managed.go`, `events.go`, `approvals.go`: turn execution and private session state.
- `store.go`, `memorystore.go`, `filestore.go`, `fileevents.go`: storage contracts and implementations.
- Feature files such as `settings.go`, `resources.go`, `hooks.go`, `workspace.go`,
  `sourcecontrol.go` and `context.go`: the associated manager operations.

`managed`, its locks and helper methods remain unexported. Go consumers cannot
access them. We do not hide a whole package just because it contains private code.

## Rules for changes

1. Start supporting packages in `internal`. Expose one only for a concrete
   embedding use case and document the contract here.
2. Keep `cmd` entry points small. Process/version concerns belong there; command
   policy and native dependency selection belong in `internal/cli`.
3. Keep session orchestration independent of RPC and concrete runtimes. The host
   selects adapters; implementations depend on the contracts they satisfy.
4. Keep the portable public packages free of process-spawning dependencies,
   including transitive ones. Native adapters are explicit opt-ins.
5. Keep provider SDK dependencies in their own modules. Neither generated client
   imports the core module.
6. Keep shared state and its synchronization together. Use files before creating
   another package solely to shorten a file.

`make architecture-check` checks the public package inventory, command/generator
isolation, session dependency direction, test-helper isolation, and transitive
process dependencies. It also runs under `make test` and the existing CI Go job.
`make wasm-check` validates both WebAssembly targets; independent example modules
exercise the actual public import boundary. `make check-generated` guards the
wire schema and clients.

## Research behind this layout

Reviewed against primary sources on 6 November 2026:

| Source                                                                                                                                                                                          | Finding and application here                                                                                                                                                                                 |
| ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| [Go: Organizing a Go module](https://go.dev/doc/modules/layout)                                                                                                                                 | Distinguishes libraries, commands, mixed repositories and standalone servers. We use its mixed layout: public library packages, private support packages, and `cmd` entry points.                            |
| [Go: internal packages](https://go.dev/doc/go1.4#internalpackages)                                                                                                                              | Import restrictions follow the parent of the `internal` directory. Moving an embedding API there would prevent external consumers from importing it.                                                         |
| [Go: Package names](https://go.dev/blog/package-names)                                                                                                                                          | Package boundaries should describe cohesive functionality; generic `util`, `common` and `misc` packages obscure ownership. We use domain names and avoid layers added only for appearance.                   |
| [Go: Code Review Comments—interfaces](https://go.dev/wiki/CodeReviewComments#interfaces)                                                                                                        | Interfaces generally belong to their consumers and should reflect actual needs. Existing runtime, store, workspace and hook boundaries remain injectable; no interface is added merely to wrap every struct. |
| [Official MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk), [internal connection implementation](https://github.com/modelcontextprotocol/go-sdk/blob/main/internal/jsonrpc2/conn.go) | Exposes client/server and transport APIs while hiding connection machinery. This is the closest protocol-SDK comparison for the public RPC host and private dispatcher.                                      |
| [Temporal Go SDK client](https://github.com/temporalio/sdk-go/blob/master/client/client.go)                                                                                                     | Uses a public client surface over internal implementation. It demonstrates that SDK API ownership need not mirror implementation location; we keep a small explicit RPC wrapper.                             |
| [NATS executable](https://github.com/nats-io/nats-server/blob/main/main.go), [server package](https://github.com/nats-io/nats-server/blob/main/server/server.go)                                | An executable can coexist with an importable server implementation. An embeddable component does not become private merely because a binary uses it.                                                         |

These projects illustrate different trade-offs, not one mandatory template. The
chosen boundaries follow this kit's consumers and dependency graph.

## Compatibility

This cleanup preserves public Go import paths, the CLI installation paths,
`-X main.version` release injection, settings/session JSON, RPC methods and the
generated Go/TypeScript contracts. No public package needs a migration for this
refactor. Tests and implementation sources moved; historical release tags are
unchanged.

For consumers of the _earlier_ flat, pre-core source layout:

| Earlier API                              | Current API                                                         |
| ---------------------------------------- | ------------------------------------------------------------------- |
| `core/contextwindow`                     | `core/model/contextwindow`                                          |
| `budget.Limits`                          | `config.BudgetLimits`                                               |
| `budget.Usage`, `budget.Exceeded`        | `event.BudgetUsage`, `event.BudgetExceededData`                     |
| `budget.Ledger`, `Metric`, `Observation` | Internal accounting; preserve opaque checkpoints                    |
| `core/rpc/client`                        | Standalone `github.com/tomanagle/harness-kit/packages/client-go`    |
| `core/process`                           | Go client's `Spawn`, or application-owned supervision and `Connect` |
