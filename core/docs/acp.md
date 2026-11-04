# Agent Client Protocol adapter

`runtimes/acp` runs an arbitrary ACP agent as a child process and translates its
events into Harness Kit events. It uses the stable ACP v1 protocol and the Go
standard library. The host selects the executable, arguments, environment,
session directory, authentication method and permissions. The adapter does not
install an agent or choose one for the application.

```go
import (
    "time"
    "github.com/tomanagle/harness-kit/core/runtimes/acp"
)

runtime := acp.New("/absolute/path/to/your-agent",
    acp.WithName("my-agent"),
    acp.WithArgs("--acp"), // Use the selected agent's documented ACP arguments.
    acp.WithEnv(map[string]string{"AGENT_TOKEN": "${AGENT_TOKEN}"}),
    acp.WithStartTimeout(30*time.Second),
    acp.WithCancelTimeout(5*time.Second),
)
// Register runtime with the same registry used for Codex and Claude Code.
```

Each session owns one process. `SessionConfig.Cwd` becomes the process directory
and the absolute ACP workspace directory. Environment overrides are resolved
when the process starts; `${NAME}` and `${NAME:-default}` use the parent's
environment. Keep secret references in saved settings rather than literal keys.
Changing process options affects subsequently started sessions; existing
processes retain their configuration.

The sidecar exposes the same launch options in the unified startup settings
document. For example, pass this to `stdio({ settings })` or `serve --settings`:

```json
{
  "process": {
    "runtimes": ["my-agent"],
    "acp": {
      "my-agent": {
        "command": "/absolute/path/to/your-agent",
        "args": ["--acp"],
        "env": { "AGENT_TOKEN": "${AGENT_TOKEN}" },
        "startTimeoutMs": 30000,
        "cancelTimeoutMs": 5000,
        "maxMessageBytes": 33554432,
        "maxImageBytes": 16777216,
        "maxRetainedOutputBytes": 8388608
      }
    }
  }
}
```

Choose the command and arguments documented by the agent you install. Omit
numeric fields to use the adapter defaults shown above; explicit values must be
positive. `process.acp` replaces the whole map, and its settings descriptor marks
it as requiring a sidecar restart. Session model, effort and native controls
remain configurable between turns through `settings.configure`.

`Discover` launches the configured command and performs only `initialize`. It
does not create a session or call a model. It reports the agent's own version and
negotiated resume/image capabilities. ACP initialization does not report whether
the agent is signed in, so authentication remains `unknown`. To authenticate
explicitly, set `options.authMethod` to an ID advertised by the agent; this sends
`authenticate` before session creation. Agents may require credentials or an
external sign-in flow before this succeeds.

## Settings interfaces and per-turn changes

Settings use the kit's existing session and turn configuration objects:

```json
{
  "model": "agent-model-id",
  "effort": "agent-effort-id",
  "options": {
    "config": { "mode": "code", "verbose": false }
  }
}
```

Model and effort values are agent-native IDs. The adapter finds their controls
by the ACP `model` and `thought_level` categories. If an agent omits these optional
categories, set startup options `modelConfigId` and `effortConfigId` to its actual
control IDs. Every advertised select/boolean control can also be addressed through
`options.config`, including modes and agent-specific settings. The adapter rejects
unknown controls, unsupported types and unavailable choices. Portable model or
effort settings fail explicitly when no matching control exists.

An embedded Go host can call `(*acp.Session).ConfigOptions()` to get an independent
snapshot for its settings UI. `runtime.status` events with status `config_options`
carry subsequent native updates. Select choices may contain groups, and changing
the model can change the available effort choices. Choices are checked again
after each acknowledged configuration change. The agent's native configuration
is applied before the prompt begins; changing an active prompt is not supported.

The native startup defaults are saved through `agent.DefaultsProvider`. Every turn
resolves a complete selection, so a one-turn override does not silently become
the next turn's default, including after unloading and resuming the session.
`options.config` is replaced as a whole by the kit's configuration API; omitted
native IDs inherit captured native defaults. Authentication and control-ID
mappings require a new session. Runtime configuration capabilities describe
which settings the adapter accepts; the selected agent must advertise the
corresponding native controls when the session starts.

ACP v1 does not provide a portable system-instructions field. Supplying
`SessionConfig.Instructions` returns `agent.ErrUnsupported`; applications can use
the selected agent's documented launch/configuration mechanism.

## Sessions, events and cancellation

The adapter negotiates protocol v1 and rejects incompatible versions. It uses
`session/new` for new conversations. With a resume ID, it prefers the negotiated
`session/resume` capability and otherwise uses `session/load` when supported.
Native history replay during `session/load` is suppressed because Harness Kit's
event store already owns the transcript; this avoids duplicated messages on
memory eviction and wakeup. Importing a transcript that exists only in an
external agent's store is not currently implemented.

Text and thought chunks map to message/reasoning deltas and completed snapshots.
Message boundaries use native message IDs when present and tool/turn boundaries
otherwise. Tool calls map to start, output and completion events. Plans and other
session metadata map to runtime status. Opt into `rawEvents` to retain unmapped
content, including native tool diffs and terminal references.

Image inputs are sent as base64 only after the agent advertises image support.
They are read from local paths; relative paths resolve against the session
directory. Unsupported content types fail before prompting.

ACP advertises `capabilities.usage: false` because it has no consumed-token
counter; nonzero token budgets are rejected as unsupported.

ACP `usage_update` reports **current context occupancy** and **cumulative session
cost**, not billable input/output token totals. Harness Kit emits the context
window and USD session cost in `usage`; it preserves native `used`, `size` and
currency in a `runtime.status` event with status `context_usage`. It does not
invent token totals or convert non-USD currencies. Consequently token-budget
enforcement requires an agent/protocol that reports billable token usage; ACP v1
context occupancy alone cannot provide it. Usage events are optional at the
agent's discretion.

Cancelling a prompt sends `session/cancel`, cancels pending host approvals and
callbacks, and waits for the original prompt response. If cancellation or its
pipe write exceeds the configured timeout, the process tree is killed and the
adapter returns `agent.ErrSessionDead`. Concurrent prompts are rejected. Closing
the session closes process pipes, uses optional native `session/close` when
negotiated, and kills an unresponsive child after the same bounded timeout.

Permission requests expose the available `allow_once`, `reject_once` and cancel
choices through the kit's approval handler. An Abort decision cancels the turn.
ACP `allow_always` and `reject_always` have agent-defined scope and lifetime, so
they are **not** mapped to the kit's session-scoped permission. Unsupported
decisions and input rewrites return a cancelled permission outcome. The adapter
never silently expands the lifetime of consent.

## Optional host capabilities

By default the adapter advertises neither filesystem nor terminal access.
The agent may still use its own tools according to its own configuration.

`WithFileSystem(acp.FileSystem{...})` enables individual `ReadTextFile` and
`WriteTextFile` callbacks. `WithTerminal(acp.TerminalHost)` enables the complete
create/output/wait/kill/release surface. The terminal host must also implement
`ReleaseSession` to clean up resources an agent forgot to release.

Callbacks receive the native session ID and trusted session root, validate
their own access policy, and must honor context cancellation. The adapter checks
session identity and absolute paths, but a host must handle root containment,
symlinks, sensitive execution/configuration files, command/environment policy
and bounded terminal output. These operations occur in the host environment;
an agent-side permission decision does not replace the host's own checks.
No unrestricted filesystem or shell implementation is supplied.

## MCP and resource limits

Portable MCP stdio definitions become ACP stdio server entries. HTTP definitions
require the agent's negotiated `mcpCapabilities.http`. Environment and header
references are expanded just before sending. ACP cannot represent per-server
working directories, tool filters, approval requirements, timeouts or disabling
native MCP inheritance; these settings return `agent.ErrUnsupported` instead of
being ignored. A host-tool bridge using HTTP likewise needs an agent that
supports HTTP MCP.

Three constructor options control adapter buffering:

| Option                  | Default | Scope                               |
| ----------------------- | ------- | ----------------------------------- |
| `WithMaxMessageSize`    | 32 MiB  | One inbound JSON line               |
| `WithMaxImageBytes`     | 16 MiB  | One image before base64 encoding    |
| `WithMaxRetainedOutput` | 8 MiB   | One completed text or tool snapshot |

Streaming text deltas remain complete when a completed snapshot reaches its
limit, and a status event reports text snapshot truncation. Completed tool
buffers are released immediately. These limits cover adapter buffers; the
agent's own memory, the session event store and subscriber queues have separate
lifetimes and controls. A message already accepted by the JSON transport and
base64 encoding also require transient allocations.

## Validation and protocol references

`go test -race ./runtimes/acp` exercises a real mock child process with deterministic
stdio messages: initialization, capability rejection, session creation/resume/
load, configuration resets, approval outcomes, cancellation, crashes, images,
filesystem opt-in and terminal cleanup. Cross-compile the tests with
`GOOS=windows GOARCH=amd64 go test -c ./runtimes/acp` to verify the Windows build.

The optional `TestLiveACP` smoke test is disabled by default. Set
`HARNESS_KIT_TEST_ACP_COMMAND` and optionally `HARNESS_KIT_TEST_ACP_ARGS` (a JSON
array) to exercise an installed, authenticated agent. Running it can consume
model quota. The deterministic suite does not claim compatibility with every
vendor's implementation or require any live account.

Implementation follows the official stable v1 documentation:
[initialization](https://agentclientprotocol.com/protocol/v1/initialization),
[stdio transport](https://agentclientprotocol.com/protocol/v1/transports),
[session lifecycle](https://agentclientprotocol.com/protocol/v1/session-setup),
[prompt updates and usage](https://agentclientprotocol.com/protocol/v1/prompt-turn),
[configuration options](https://agentclientprotocol.com/protocol/v1/session-config-options),
[tool calls and permissions](https://agentclientprotocol.com/protocol/v1/tool-calls),
[filesystem](https://agentclientprotocol.com/protocol/v1/file-system), and
[terminals](https://agentclientprotocol.com/protocol/v1/terminals).
