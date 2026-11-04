# Host tools

Host tools are functions in *your* application that agents can call:
create a ticket, look up a customer, open a panel in your UI. Codex and
Claude Code already bring their general-purpose tools (shell, files,
search, web). Harness Kit adds a way for your app's own tools to reach
every runtime, without any per-runtime code.

```ts
// Electron main / Node, through the sidecar
await kit.tools.register({
  name: "create_ticket",
  description: "Create a support ticket",
  inputSchema: { type: "object", properties: { title: { type: "string" } }, required: ["title"] },
  approval: "ask",                                  // ask the user before each call
  handler: async ({ title }, ctx) => tickets.create(title, { signal: ctx.signal }),
});
const session = await kit.sessions.create({ runtime: "codex" });   // or "claude"
```

```go
// Go, in-process
reg := tools.NewRegistry()
tools.Add(reg, "create_ticket", "Create a support ticket",
	func(ctx context.Context, in struct {
		Title string `json:"title" desc:"One-line summary"`
	}) (Ticket, error) {
		return tickets.Create(ctx, in.Title)
	}, tools.WithApproval(mcp.ApprovalAsk))

host := toolhost.New(toolhost.Options{Registry: reg})
defer host.Close()
mgr := session.NewManager(registry, session.Options{ToolServer: host})
// and first thing in main(): toolhost.ServeBridgeIfRequested()
```

Both runtimes were checked live: a TypeScript-registered tool was called by
Codex (as `harness_kit/<tool>`) and by Claude Code (as
`mcp__harness_kit__<tool>`), with the handler running in the Node process.

## How it works

```text
runtime ──stdio──► bridge (harness-kit, relay only) ──Unix socket──► tool host (your process)
                                                                     MCP server: tools/list, tools/call
                                                                     ├─ validates arguments
                                                                     ├─ approval: ask → approval.requested
                                                                     └─ runs the handler (Go, or tool.call → your TS app)
```

- Each session whose runtime supports MCP gets one extra MCP server,
  `harness_kit`, added to its per-session configuration (the same
  mechanism as [mcp.md](mcp.md)). It is only added when there is something
  to serve: registered tools, or Claude Code's permission prompts.
- The runtime launches the bridge, which is the `harness-kit` binary (or
  any executable calling `toolhost.ServeBridgeIfRequested()` first in
  `main`). The bridge only relays bytes to the tool host's Unix socket.
  The socket sits in a private (0700) temporary directory, and each session
  presents its own random token. No network port is opened; Go's `unix`
  sockets work on macOS, Linux and Windows 10+.
- The bridge details (socket, token) are only in the runtime's per-session
  configuration. They are not stored in session records, and for Claude
  Code they are in a 0600 config file, not on the command line.
- Handlers run in the host process. Sidecar clients' handlers run in the
  client: the sidecar sends a `tool.call` request and forwards cancellation
  as `$/cancelRequest` (an `AbortSignal` in TypeScript).

## Tool behaviour

| | |
| --- | --- |
| Input | validated against the tool's JSON Schema before the handler runs; invalid arguments return an error result explaining why |
| Schema (Go) | inferred from the argument struct: `json` names; `omitempty` and pointer fields are optional; `desc:"..."` and `enum:"a,b"` tags |
| Output | a string becomes text; a `tools.Result` / `ToolResult` is passed through; other values become JSON text plus `structuredContent` |
| Errors | handler errors, panics and timeouts become error results the model can read (`isError: true`); the turn continues |
| `approval: "ask"` | Harness Kit asks before every call (`approval.requested`, policies apply). `deny` returns an error result; `abort` also cancels the turn. Works on both runtimes. |
| Timeout | `timeoutMs` / `tools.WithTimeout`, default 2 minutes |
| Concurrency | `tools.MaxConcurrent(n)` across the registry |
| Read-only | `readOnly: true` is passed to runtimes as MCP's `readOnlyHint` |

Per session: `hostTools: ["a", "b"]` limits a session to some tools;
`disableHostTools: true` gives it none (and no bridge). Register tools
before creating the sessions that should use them. Runtimes list tools when
a session starts, and the host sends `notifications/tools/list_changed`
when the registry changes, but not every runtime refreshes on it.

A client's tools are removed when it disconnects, and clients can only
unregister their own tools.

## Claude Code approvals

The same bridge answers Claude Code's permission prompts
(`--permission-prompt-tool`, documented by Anthropic for non-SDK hosts).
Every Claude Code session with a tool host routes "may I run this?" prompts
through the session's normal approval flow:

- `Bash` / `PowerShell` → `kind: command` with `command`
- `Edit` / `Write` / `MultiEdit` / `NotebookEdit` → `kind: file_change` with `paths`
- `WebFetch` / `WebSearch` → `kind: network`
- MCP and other tools → `kind: tool`

Allow, deny and abort work (allow-for-session does not; Claude Code doesn't
offer it to prompt tools). The contract was checked against Claude Code
2.1.316: the tool receives `{tool_name, input, tool_use_id}` and answers
`{"behavior":"allow","updatedInput":…}` or `{"behavior":"deny","message":…}`.
Claude Code hides the prompt tool from the model.

The CLI and the sidecar always have a tool host, so Claude Code approvals
work in both. In Go, set `session.Options.ToolServer`. Without one, Claude
Code sessions fall back to `--permission-prompts none` (anything that would
prompt is denied).

## Protocol

| Method | Direction | |
| --- | --- | --- |
| `tools.register` `{tools: [ToolSpec]}` | client → server | all or nothing; duplicate names fail with `duplicate_tool` |
| `tools.unregister` `{names}` | client → server | only the caller's own tools |
| `tools.list` | client → server | every registered tool |
| `tool.call` `{sessionId, turnId, name, arguments}` | **server → client** | reply with a ToolResult; `$/cancelRequest` if abandoned |

`ToolSpec = {name, description?, inputSchema?, approval?: "auto"|"ask", readOnly?, timeoutMs?}`.
`capabilities.tools` in `initialize` reports whether the server hosts
tools. See [protocol.md](protocol.md).

## What the kit deliberately doesn't do

- It offers no built-in shell or filesystem tools. The runtimes have their
  own, under their own sandboxes, and a sidecar must not expose
  unrestricted shell or file access to whoever holds its connection.
  Reference tools for your own agent loop (e.g. a file tool confined with
  Go's `os.Root`) may come later, for Go programs only.
- It doesn't use Codex's experimental "dynamic tools" or Claude Code's
  SDK-only custom tools. MCP is the stable path into both.
