# Calling models directly

`model.Provider` is the build-your-own track. It streams inference and leaves the
agent loop, tools, approvals, persistence and UI to the application. Delegated
Codex/Claude Code sessions continue using `agent.Runtime`; this track does not
require their CLIs or change their runtime-owned context.

The core `model` and `model/contextwindow` packages have no external dependencies and
build for WebAssembly. SDK adapters live in separate Go modules:

| Module | API | Minimum Go |
| --- | --- | --- |
| `github.com/tomanagle/harness-kit/core/providers/openai` | Official OpenAI Go SDK v3.93.0, Responses streaming | 1.26 |
| `github.com/tomanagle/harness-kit/core/providers/anthropic` | Official Anthropic Go SDK v1.84.0, Messages streaming | 1.24 |

Choose only the adapter your application needs. SDK dependencies do not enter
the core module. These are the new monorepo module paths and are not tagged yet;
use a source checkout and the included example's local module replacements until
new releases exist. Future provider tags use `core/providers/openai/v…` and
`core/providers/anthropic/v…`. Historical provider releases keep their old paths.
See [migration and releases](releases.md#monorepo-paths-and-go-migration). Provider capability flags describe adapter features; individual
models can impose further restrictions, which the API reports as errors.

## One settings object, per-request snapshots

`model.Request.Settings` uses the same `config.Settings` as delegated sessions:
`Model`, `Effort` and provider-native JSON `Options`. The caller chooses the model;
there is no default paid model. Build an independent request from your current
settings before each API request. UI updates then apply to the next request
without recreating the provider or restarting a process. They do not alter an
already-running HTTP request; cancel its context to stop it.

`MaxOutputTokens` is an explicit request limit (required by Anthropic). Hosts can
keep it and `contextwindow.Config` alongside `config.Settings` in their one
settings document, as the [model example](../../examples/model) does. `config.Apply` supports inherited
session defaults and per-request patches. Configuration does not read files or
environment variables on the application's behalf.

Native options use API field names, preserve `false`, zero and `null`, and replace
whole field values. For example, OpenAI `store`, `include` and `text`, or Anthropic
`thinking` and `stop_sequences`, can be passed without extending the core.
Options cannot overwrite portable request fields such as model, messages, tools
or token limits; conflicting portable/native effort settings return an error.
Model-specific validity remains the provider API's responsibility.

```go
// import hkopenai "github.com/tomanagle/harness-kit/core/providers/openai"
p := hkopenai.New()
req := model.Request{
    Settings: config.Settings{Model: selectedModel},
    Messages: []model.Message{model.Text(model.User, prompt)},
    MaxOutputTokens: 1024,
}
reply, usage, err := model.Collect(ctx, p, req, func(ev model.Event) bool {
    if ev.Type == model.TextDelta { fmt.Print(ev.Text) }
    return true
})
```

Constructors accept the official SDK's request options, including HTTP clients,
base URLs, API keys and retries; `NewWithClient` accepts an already-configured SDK
client. Default SDK environment configuration supports `OPENAI_API_KEY` or
`ANTHROPIC_API_KEY`. Keys belong to the host, never session metadata or serializable
kit settings. Retries default to zero to avoid silently replaying ambiguous paid
requests; hosts may explicitly enable SDK retries. These SDK modules target a
trusted backend/desktop host, not a browser carrying a secret key.

## Streams and continuation

Streams are `iter.Seq2[model.Event, error]`. They carry text and visible reasoning
deltas, tool-call argument fragments, complete calls, cumulative usage and final
assistant messages. `ArgumentsDelta` is an unparsed string fragment; only
`ToolCallComplete.Call.Arguments` is complete JSON. Do not execute a partial call.
Breaking iteration closes the HTTP stream. Context cancellation propagates to
the SDK; HTTP/API errors propagate to the caller. EOF without a completion event
returns `model.ErrIncomplete`.

Completion includes a finish reason. Output-limit stops are completed responses
with their provider's stop reason, not transport errors; applications decide
whether to continue. `Collect` keeps the last cumulative usage value rather than
adding repeated snapshots. Input usage includes cached tokens in both adapters;
cache read/write values are subsets, not additional input tokens. OpenAI reasoning
tokens are a subset of output tokens. Usage is not a cost estimate.

Final messages retain provider-native output blocks alongside portable content.
Keep `Message.Native` when appending replies or serializing history: it preserves
OpenAI encrypted reasoning/phase fields and Anthropic thinking signatures. The
matching adapter replays those blocks verbatim. Do not edit portable content while
leaving stale native blocks attached. Cross-provider native replay is rejected;
applications must explicitly transform history if switching providers.

Tools are JSON Schema definitions. Applications execute calls themselves, then
append `model.Result(call.ID, text, isError)`. Every pending call needs exactly one
result before inference continues. `tools.Registry` provides shared validation,
timeouts and handlers; a direct loop must implement approvals before calling it.
The example denies tools marked `ApprovalAsk` until a host supplies an interface.
Images accept a URL or base64 data plus media type; the kit does not load files or
fetch image URLs. Anthropic requires text-only system/developer instructions at
the start of portable history. Unsupported input returns an explicit error.

## Context ownership and memory

`contextwindow.Window` (import `github.com/tomanagle/harness-kit/core/model/contextwindow`) stores an application's raw-model conversation and exposes
JSON-serializable, independent snapshots. Save snapshots wherever your application
chooses, discard the Window to release its references, and `Restore` it when needed.
It does not own an automatic disk cache. Delegated session memory/eviction settings
belong to the separate session manager.

`contextwindow.Fit` takes a request, limits and a caller-supplied estimator:

```go
prepared, err := contextwindow.Fit(ctx, req, contextwindow.Config{
    MaxTokens: contextLimit,
    ReservedOutputTokens: outputReserve,
    KeepRecentTurns: 2,
}, estimator, nil)
```

The output reserve is at least `req.MaxOutputTokens`. Zero context limit disables
trimming. The default strategy removes oldest complete user turns, including
their full tool exchanges, while retaining all system/developer instructions and
at least the latest turn. If protected content still exceeds the limit, it returns
`ErrBudget`. A custom compactor may replace older content with summaries; its
result is validated, recounted and checked for preserved instructions/recent turns.

Estimators see the complete request, including schemas, images and native blocks.
Use a model-aware tokenizer or provider count endpoint for accurate limits, and
set `Estimate.Exact` accordingly. The supplied `ApproximateJSON(bytesPerToken)` is
explicitly a coarse byte heuristic: it can overcount base64 and underestimate
model-specific image costs. It cannot guarantee a model's context window or bound
process RSS. Compaction can change model behavior; callers choose that policy.

`Window.Replace(snapshot.Revision, prepared.Request.Messages)` commits compacted
history only if no concurrent append changed it. A conflict returns `ErrConflict`
instead of dropping a newly submitted prompt. The application owns serialization
of inference/tool loops and any retry policy.

## Runnable example and validation

From the repository root, enter `examples/model` and run with an explicitly
chosen model and the appropriate SDK environment credential. This standalone
example module is `github.com/tomanagle/harness-kit/examples/model`; its local
replacements resolve the core in `../../core` and SDK adapters in
`../../core/providers/`.

```sh
cd examples/model
go run . -provider openai -model "$MODEL" -prompt 'What is the UTC time?' -max-steps 4
go run . -provider anthropic -model "$MODEL" -prompt 'What is the UTC time?' -max-output 1024
```

The example refreshes its settings snapshot before each inference, uses the
shared tool registry, handles tool errors and supports cancellation and configurable
request/context limits. Running it makes paid API requests. Automated tests use
local HTTP servers with real SDK request/stream code; they do not contact paid
endpoints. Provider availability, model compatibility and live quotas require
separate opt-in integration testing with a host's credentials. Run
`make test-examples` from the repository root or `core/` to validate the separate
Go example modules.

Official references: [OpenAI streaming Responses](https://developers.openai.com/api/docs/guides/streaming-responses),
[OpenAI tool calling](https://developers.openai.com/api/docs/guides/function-calling),
[OpenAI Go SDK](https://github.com/openai/openai-go),
[Anthropic streaming](https://platform.claude.com/docs/en/build-with-claude/streaming),
and [Anthropic Go SDK](https://github.com/anthropics/anthropic-sdk-go).
