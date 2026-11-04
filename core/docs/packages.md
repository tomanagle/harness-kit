# Public packages and internal implementation

The public Go surface contains contracts and building blocks a harness author
can compose. Implementation packages that exist only to support those APIs live
under `internal/`; Go prevents applications outside the core module from
importing them.

| Area | Public packages | Purpose |
| --- | --- | --- |
| Runtime harness | `agent`, `session`, `event`, `permission` | Runtime adapters, lifecycle, stores, event streams and approvals |
| Configuration | `config`, `settings` | Shared values, patches, budget limits and setting lifetimes |
| Host tools | `tools`, `toolhost`, `mcp`, `mcp/client` | Tool execution, serving tools to runtimes, and connecting to MCP servers |
| Direct inference | `model`, `model/contextwindow` | Provider contracts and caller-owned history, token estimation and compaction |
| Workspaces | `workspace`, `workspace/git` | Portable workspace contracts and the optional native Git backend |
| Protocol hosting | `rpc/protocol`, `rpc/server` | Wire definitions and serving a manager over an application-owned transport |
| Native runtimes | `runtimes/codex`, `runtimes/claude`, `runtimes/acp`, `runtimes/fake` | Optional runtime implementations |

`model/contextwindow` stays public because builders using direct model providers
own their inference loop and conversation history. They can choose an estimator,
compactor, retention limits and storage. Delegated runtime sessions retain the
runtime's own history management. `mcp/client` also stays public: embedded
applications can connect to an MCP server, inspect its tools and invoke them.

The implementation packages are:

- `internal/budget`: usage accounting, snapshot/delta reconciliation and durable checkpoints.
- `internal/process`: native executable lookup and process-tree supervision.
- `internal/jsonrpc`: transport framing and request dispatch.
- `internal/rpcclient`: the CLI's private client using core-domain types.

Application sidecar clients live at repository-root `packages/client-go` and
`packages/client-ts`; their wire types and RPC methods are generated from the
shared schema. The Go client is a separate module and does not depend on core.

## Migrating the earlier flat source layout

These changes apply to the current, unreleased `github.com/tomanagle/harness-kit/core`
module. Historical release tags remain unchanged.

| Earlier source | Current API |
| --- | --- |
| `core/contextwindow` | `core/model/contextwindow`, with the same API |
| `budget.Limits` | `config.BudgetLimits` |
| `budget.Usage` | `event.BudgetUsage` |
| `budget.Exceeded` | `event.BudgetExceededData` |
| `budget.Ledger`, `Metric`, `Observation` | Private accounting implementation |
| `core/rpc/client` | Standalone `github.com/tomanagle/harness-kit/packages/client-go` for applications |
| `core/process` | Private; use the Go client's `Spawn`, or supervise your own process and pass streams to `Connect` |

Custom session stores preserve `Record.Accounting` as opaque `json.RawMessage`.
It remains the same JSON object on disk, so existing checkpoints can still be
restored. Stores do not need to import accounting internals or interpret the
checkpoint. Public session information continues to expose observed usage.

Hosts relaying raw protocol frames, such as the Wails example, own their process
supervision. The example contains small platform helpers rather than importing
the kit's private process implementation.
