# Runtimes

A runtime is an existing, complete agent harness that Harness Kit drives.
Every adapter uses the provider's own documented integration surface. No
adapter reads credentials, scrapes a terminal, or impersonates a client.

| | Codex | Claude Code | fake |
| --- | --- | --- | --- |
| Integration | `codex app-server` (JSON-RPC over stdio) | `claude -p --output-format stream-json` | in-process |
| Verified against | codex-cli 0.166.0 | Claude Code 2.1.316 | — |
| Streaming | ✓ | ✓ | ✓ |
| Interactive approvals | ✓ (commands, file changes, permission grants, MCP tools) | ✓ via `--permission-prompt-tool` and Harness Kit's bridge ([tools.md](tools.md#claude-code-approvals)) | ✓ |
| Host tools ([tools.md](tools.md)) | ✓ | ✓ | ✓ (`[call NAME {args}]`) |
| Cancel | `turn/interrupt` | SIGINT, then kill | ✓ |
| Resume | `thread/resume` | `--resume` | ✓ |
| Image input | local image references | PNG, JPEG, GIF, WebP via stream-json stdin | — |
| Usage / cost | tokens / – | tokens / estimated USD | ✓ |
| Sign-in check | `account/read` | `claude auth status --json` | – |
| List stored sessions | `thread/list` | ✗ (SDK-only; see below) | this process's sessions |
| Per-session MCP servers ([mcp.md](mcp.md)) | ✓ thread `config.mcp_servers` | ✓ `--mcp-config` (`approval: ask` not yet) | reports what it received |

"Verified" means the adapter's unit tests run against a scripted fake of
the CLI. The opt-in live tests (`make test-live`) also passed against the
real, signed-in CLIs at these versions: two turns each, including a resume.

## Authentication and usage policies

Harness Kit never reads, stores or forwards credentials, and never offers a
login flow. Users sign in with each CLI's own command, and Harness Kit only
asks the CLI whether it is signed in. These are the provider rules as of
November 2026. Builders of products must read them; `runtime.discover`
repeats the short version in `notes`.

### OpenAI Codex

From the [Codex app-server documentation](https://learn.chatgpt.com/docs/app-server):

> If you've built a local or open-source application using Codex app-server
> authentication, you can continue using it, though we recommend migrating
> to Sign in with ChatGPT so users have greater control over and visibility
> into their usage. … App-server authentication has never been permitted
> for commercial or hosted services. We launched Sign in with ChatGPT to
> support these use cases.

The same page asks integrations to set `clientInfo` to identify themselves
("`clientInfo.name` identifies your client to the OpenAI Compliance Logs
Platform"), and asks enterprise integrations to contact OpenAI to be added
to a known-clients list.

What this means for you:

- **Local or open-source apps** may drive the user's own `codex login`
  session through the app-server. This is what Harness Kit does.
- **Commercial or hosted products** must not rely on app-server
  authentication. Use OpenAI's *Sign in with ChatGPT* program or API keys.
- Set your own client identity:
  `codex.New(codex.WithClientInfo(codex.ClientInfo{Name: "my_app", Title: "My App", Version: "1.0.0"}))`.

### Anthropic Claude Code

From Claude Code's [Legal and compliance](https://code.claude.com/docs/en/legal-and-compliance) page:

> Developers building products or services that interact with Claude's
> capabilities, including those using the Agent SDK, should use API key
> authentication through Claude Console or a supported cloud provider.
> Anthropic does not permit third-party developers to offer Claude.ai login
> into their own applications, or to route requests through Free, Pro, Max,
> or Ultra plan credentials on behalf of their users. Moreover, developers
> may not collect, store, or intermediate Claude.ai credentials or session
> tokens …
>
> Nor does it prevent an end user from signing in to the unmodified Claude
> Code binary with their own Claude subscription …

The same page requires products that run Claude Code to leave the binary
unmodified, not remove its authentication methods, and not "pay for,
resell, or intermediate Claude usage". Each end user authenticates with
their own credentials. The [Agent SDK overview](https://code.claude.com/docs/en/agent-sdk/overview)
adds: "Unless previously approved, Anthropic does not allow third party
developers to offer claude.ai login or rate limits for their products …".

On billing, the help-centre article
[Use the Claude Agent SDK with your Claude plan](https://support.claude.com/en/articles/15036540-use-the-claude-agent-sdk-with-your-claude-plan)
currently states that previously announced changes are paused, and that
"Claude Agent SDK, `claude -p`, and third-party app usage still draw from
your subscription limits". Check it again before relying on it, because
this has changed during 2026.

What this means for you:

- **A user running your tool on their own machine, with their own signed-in
  Claude Code,** is the case the terms describe as permitted. Harness Kit
  runs the unmodified `claude` binary with whatever authentication the user
  configured.
- **A product you ship to other people** should authenticate with API keys:
  each user's own key, or your Claude Console or cloud-provider
  credentials, billed under your agreement. Pass the key through the
  environment, e.g. `claude.New(claude.WithEnv("ANTHROPIC_API_KEY=" + key))`,
  and consider `bare: true`, which never reads OAuth credentials. Do not
  build a "Sign in with Claude" flow.
- Don't name your product "Claude Code" or imitate its branding (see the
  Agent SDK branding guidelines).

## Codex adapter (`runtimes/codex`)

Each session starts a dedicated `codex app-server` process on stdio and
performs the documented handshake (`initialize` with `clientInfo`, then
`initialized`). It then calls `thread/start` or
`thread/resume {threadId, excludeTurns: true}`. A turn is `turn/start`,
followed by notifications until `turn/completed`. Messages use the
documented "lite" framing with no `jsonrpc` member.

Portable `model` and `effort` are accepted at creation, through
`session.configure`, and as per-turn overrides. Native default resolution,
reset behavior and capability discovery are described in
[Configuration](configuration.md). Codex-specific options below are startup-only.
Use portable model/effort rather than duplicating them in native `config` keys.

Session options (`options` in `session.create`):

| option | values | default |
| --- | --- | --- |
| `approvalPolicy` | `untrusted`, `on-request`, `never` | inherit Codex configuration |
| `sandbox` | `read-only`, `workspace-write`, `danger-full-access`* | inherit Codex configuration |
| `effort` (legacy startup option; prefer portable `effort`) | model-advertised effort, e.g. `low`, `high` | Codex default |
| `ephemeral` | do not persist the thread | `false` |
| `config` | thread config overrides, passed through (keys touching sandbox or approval settings*) | — |

\* These require `AllowUnsafe` / `--allow-unsafe`.

Event mapping:

| Codex | Harness Kit |
| --- | --- |
| `item/agentMessage/delta`, `item/completed` (agentMessage) | `message.delta`, `message.completed` |
| `item/reasoning/summaryTextDelta`, `item/reasoning/textDelta`, `item/completed` (reasoning) | `reasoning.delta`, `reasoning.completed` |
| `item/started` / `item/completed` for commandExecution, fileChange, mcpToolCall, dynamicToolCall, webSearch, collabAgentToolCall | `tool.started` / `tool.completed` |
| `item/commandExecution/outputDelta`, `item/fileChange/outputDelta` | `tool.output` |
| `thread/tokenUsage/updated` | `usage` (scope `session`); `turn.completed.usage` is the per-turn delta |
| `error` with `willRetry` | `runtime.status` `retrying`; otherwise `error` |
| `warning`, `configWarning`, `deprecationNotice`, `model/rerouted`, `thread/compacted` | `runtime.status` |
| anything else | `runtime.raw` (with `rawEvents`) |

Approvals (server-to-client requests):

| Codex request | Harness Kit request | outcome mapping |
| --- | --- | --- |
| `item/commandExecution/requestApproval` | `kind: command` with `command`, `cwd`, `reason` | allow→`accept`, allow_session→`acceptForSession`, deny→`decline`, abort→`cancel` |
| `item/fileChange/requestApproval` | `kind: file_change` with `paths` | same |
| `item/permissions/requestApproval` | `kind: permissions` | allow / allow_session grant the requested profile for the turn / session; deny grants nothing |
| `mcpServer/elicitation/request` tagged `_meta.codex_approval_kind: "mcp_tool_call"` (MCP tool approval, for servers with approval mode `prompt`) | `kind: tool`, `tool: "mcp/<server>"`, the tool's params as `input` | allow → `accept`, deny → `decline`, abort → `cancel` |

Requests Harness Kit cannot meaningfully answer get safe replies, so a turn
never hangs:

- other MCP elicitations (forms a server wants filled in) → `decline`
- `item/tool/requestUserInput` → empty answers
- legacy v1 approvals → `denied`
- token refresh, attestation and dynamic tool calls → a method-not-found
  error (Harness Kit never handles tokens)

Limitations:

- Notifications from other threads (Codex subagents) appear only as
  `runtime.raw`.
- The app-server protocol is marked experimental by OpenAI. The adapter
  uses only stable (non-`experimentalApi`) methods and ignores unknown
  fields; re-verify on Codex upgrades with `make test-live`. You can
  regenerate the schema with `codex app-server generate-json-schema --out DIR`.

## Claude Code adapter (`runtimes/claude`)

Anthropic's Agent SDK documentation says: "To drive the same agent loop
from a language other than Python or TypeScript, run the CLI as a
subprocess". This adapter does exactly that.

- **Turn**: `claude -p --output-format stream-json --verbose --include-partial-messages`.
  The prompt goes on **stdin**, so there are no argv length limits and a
  prompt can never be parsed as a flag.
- **Session**: the first turn pins a generated UUID with `--session-id`;
  later turns use `--resume <id>`, as does a session created with
  `resumeId`. With `forkOnResume`, the first resumed turn adds
  `--fork-session` and the adapter adopts the new ID reported in
  `system/init`.
- **Cancel**: SIGINT, which Claude Code documents as ending the turn
  cleanly. After 5 s the process tree is killed. On Windows, where console
  interrupts are unreliable for child processes, it is killed directly.
- **Permissions**: with a tool host (always in the CLI and sidecar),
  sessions get `--permission-prompt-tool mcp__harness_kit__approval_prompt`,
  and every permission prompt becomes an `approval.requested` event
  (allow, deny, abort). Without one, Claude Code ≥ 2.1.259 gets
  `--permission-prompts none`: anything that would prompt is denied, and
  Harness Kit emits `runtime.status: permission_denied`. Either way,
  `permissionMode` and `allowedTools` decide what runs without asking.

| option | meaning |
| --- | --- |
| `permissionMode` | `default`/`manual`, `acceptEdits`, `plan`, `auto`, `dontAsk`, `bypassPermissions`* |
| `allowedTools` / `disallowedTools` | permission rules, e.g. `["Read", "Bash(git diff *)"]` |
| `tools` | restrict built-in tools; `[]` disables all |
| `addDirs` | extra directories the tools may access |
| `bare` | skip hooks, plugins, MCP servers and CLAUDE.md; **API-key auth only** |
| `maxTurns`, `maxBudgetUsd` | per-prompt limits; also configurable between turns or per turn |
| `maxImageBytes` | optional per-image byte cap before encoding; `null` inherits no kit cap, `0` refuses nonempty images; configurable per turn or between turns |
| `effort` | legacy startup option; prefer portable `effort` |
| `forkOnResume` | see above; startup-only |
| `disablePartialMessages` | disables partial message streaming; configurable per turn or between turns |

\* Requires `AllowUnsafe` / `--allow-unsafe`.

Event mapping:

- `stream_event` text and thinking deltas → `message.delta` / `reasoning.delta`
- `assistant` text / thinking / tool_use → `message.completed` / `reasoning.completed` / `tool.started`
- `user` tool_result → `tool.completed`
- `system/init` → `runtime.status: ready`
- `system/api_retry` → `retrying`
- `rate_limit_event` (when not `allowed`) → `rate_limited`
- `result` → `usage`, and the turn's status

`total_cost_usd` is cumulative for a conversation, so the adapter reports
per-turn cost as a delta. For a session resumed from an older conversation,
the first turn has no baseline and reports session cost only. Subagent
messages carry `parentCallId`.

Things to know:

- **Without `bare`, `claude -p` loads the user's whole Claude Code setup**:
  hooks, plugins, MCP servers and CLAUDE.md, including those defined in the
  working directory. Claude Code documents that a `-p` session shows no
  workspace-trust dialog. Only point sessions at directories you trust. It
  can also be large: the live test's fresh session cached about 230k tokens
  of the developer's own configuration.
- `bare` mode authenticates only with `ANTHROPIC_API_KEY` or an
  `apiKeyHelper`, never with a Claude.ai login. Anthropic notes it will
  become the default for `-p` in a future release.
- Image prompts use `--input-format stream-json` with ordered text and base64
  image content blocks. Relative paths resolve against the session's working
  directory. The kit detects the actual media type and requires regular files.
  Configure `session.options.maxImageBytes` to bound each image before it is
  read; native Claude size limits also apply. Saved `turn.started` events retain
  the input path references, not the image bytes, so applications that need
  portable attachment replay should retain those files themselves. See the
  [documented streaming input format](https://code.claude.com/docs/en/agent-sdk/streaming-vs-single-mode).
- **Listing past sessions**: Claude Code offers `listSessions()` only in its
  Python and TypeScript SDKs, and there is no CLI command for it, so the
  adapter does not implement `agent.SessionLister`. Harness Kit's own store
  (`harness-kit sessions`, or a sidecar started with `--store`) still
  records every Claude session you run through Harness Kit. Its transcripts
  live in `~/.claude/projects/`, but their format is not a documented API,
  so Harness Kit doesn't parse them.

Claude model and effort support per-turn overrides and future-turn session
defaults. Native model inheritance across resume has specific limitations for
older sessions or an override before the first native-model turn; see
[Configuration](configuration.md#native-defaults-and-honest-snapshots).
Permission/tool context remains startup-only. The adapters never restart or
cancel a turn to apply configuration changes.

**Interactive approvals** use Claude Code's documented
`--permission-prompt-tool`, served by Harness Kit's tool bridge
([tools.md](tools.md#claude-code-approvals)). The contract was verified
live: the tool gets `{tool_name, input, tool_use_id}` and answers
`{"behavior": "allow", "updatedInput": …}` or
`{"behavior": "deny", "message": …}`. Claude Code hides the tool from the
model. MCP configuration is passed in a 0600 temporary file rather than on
the command line.

## The fake runtime (`runtimes/fake`)

The fake is deterministic, offline, and needs no account. Enable it in the
sidecar with `--fake-runtime`. The prompt directives `[approve]`, `[slow]`,
`[fail]`, `[crash]` and `[raw]` exercise approvals, cancellation, failures,
dead sessions and raw events. All protocol and TypeScript client tests run
against it.

## Writing an adapter

Implement `agent.Runtime` and `agent.Session`:

```go
type Runtime struct{}

func (Runtime) Info() agent.Info { return agent.Info{Name: "mine", DisplayName: "My Agent", Capabilities: agent.Capabilities{Streaming: true, Cancel: true}} }

func (Runtime) Discover(ctx context.Context) (agent.Discovery, error) {
	// Find the binary (process.LookPath), read its version, ask it, never
	// its credential files, whether it is signed in.
	return agent.Discovery{Installed: true, Ready: true, Auth: agent.AuthStatus{State: agent.AuthUnknown}}, nil
}

func (Runtime) Start(ctx context.Context, cfg agent.SessionConfig, h agent.Handler) (agent.Session, error) {
	var opts MyOptions
	if err := agent.DecodeOptions(cfg.Options, &opts); err != nil { // rejects unknown fields
		return nil, err
	}
	return &mySession{h: h}, nil // ctx bounds startup only; the session outlives it
}

func (s *mySession) Prompt(ctx context.Context, turn agent.Turn) (agent.TurnResult, error) {
	s.h.Emit(event.Event{Type: event.MessageDelta, TurnID: turn.ID, Data: event.MessageDeltaData{ItemID: "m1", Text: "hi"}})
	d, err := s.h.RequestApproval(ctx, permission.Request{Kind: permission.KindCommand, Title: "Run `make`", Command: "make"})
	if err != nil { // no decision: treat as aborted
		return agent.TurnResult{Status: event.TurnStatusCancelled}, nil
	}
	_ = d
	// On ctx cancellation: interrupt gracefully, then return TurnStatusCancelled.
	return agent.TurnResult{Status: event.TurnStatusCompleted}, nil
}
```

Contract details:

- Emit only turn-internal events. The session manager emits
  `turn.started`/`turn.completed`, assigns `seq`, and fills in `sessionId`.
- Wrap `agent.ErrSessionDead` when the session can no longer be used, so
  the manager marks it `failed`.
- If your options can disable safety mechanisms, implement `agent.Guarded`.
- Test with a helper process: re-execute the test binary as a scripted fake
  of the CLI (see `runtimes/codex/codex_test.go`). This exercises real
  process I/O on every OS.

Runtimes not yet investigated include Cursor, OpenCode and Gemini CLI. Each
needs its official integration surface and terms reviewed before an
adapter is written. A generic ACP adapter would cover every agent that
speaks the [Agent Client Protocol](https://agentclientprotocol.com).

## Portable forks

`agent.Forker` is an optional adapter extension. `Capabilities.Fork` advertises
support, and `ForkDeferred` means a native ID is allocated only on the first real
prompt. Codex uses the official [`thread/fork`](https://learn.chatgpt.com/docs/app-server)
method (`threadId`, `excludeTurns: true`). Claude Code uses its documented
[`--resume --fork-session`](https://code.claude.com/docs/en/cli-reference) flags.
Neither adapter runs a dummy prompt. The fake adapter allocates an independent
echo conversation. See [pending-fork and history semantics](sessions.md).
