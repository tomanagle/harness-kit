# Harness Kit Go client

`github.com/tomanagle/harness-kit/packages/client-go` is the standalone,
standard-library-only client for the Harness Kit sidecar protocol. It contains
generated wire types and RPC methods plus transport, lifecycle, and callback
helpers. It does not depend on the Go server module or start an agent until your
application asks it to.

The module is currently available from this source checkout; its first release
tag is still pending. An application working from a checkout can use:

```sh
go mod edit -require=github.com/tomanagle/harness-kit/packages/client-go@v0.0.0
go mod edit -replace=github.com/tomanagle/harness-kit/packages/client-go=/absolute/path/to/harness-kit/packages/client-go
go mod tidy
```

Build the local sidecar with `make build` at the repository root. For applications
that embed a runtime in the same Go process, use the SDK in [`core/`](../../core)
instead of a protocol connection.

## Start a sidecar

```go
package main

import (
    "context"
    "encoding/json"
    "fmt"
    "log"

    hk "github.com/tomanagle/harness-kit/packages/client-go"
)

func main() {
    ctx := context.Background()
    kit, err := hk.Spawn(ctx, "./core/bin/harness-kit",
        []string{"serve", "--fake-runtime"}, nil, hk.Options{
            Settings: hk.SettingsPatch{
                "memory": json.RawMessage(`{"evictAfterIdleMs":600000}`),
            },
        })
    if err != nil { log.Fatal(err) }
    defer kit.Close()

    created, err := kit.SessionCreate(ctx, hk.CreateSessionParams{Runtime: "fake"})
    if err != nil { log.Fatal(err) }
    result, err := kit.Run(ctx, hk.SessionPromptParams{
        SessionID: created.Session.ID,
        Input: []hk.InputPart{{Type: "text", Text: "Hello"}},
    })
    if err != nil { log.Fatal(err) }
    fmt.Println(result.Status)
}
```

`Settings` supplies the same startup settings document used by other clients.
`SettingsConfigure` applies live changes, and `OnSettings` receives revisioned
updates. `InitialSettings` applies live preferences before `Connect` or `Spawn`
returns. Fields that need a new session or process restart retain those semantics;
the client does not restart anything implicitly.

## Attach to your own transport

`Connect(ctx, reader, writer, options)` accepts newline-delimited JSON over any
byte stream. Your application can construct its own process or network transport.
The initialization context only bounds initialization. Each generated method has
its own context; call `Close` to end the connection.

By default, `Connect` takes ownership of readers and writers implementing
`io.Closer`. Set `CloseTransport` to control cleanup or to interrupt custom I/O.
If neither the reader nor the cleanup function can interrupt a blocked read,
the reader goroutine cannot exit until its owner supplies data or EOF. `Close`
still fails pending requests and cancels reverse tool handlers immediately.

Closing an attached connection does **not** shut down the shared host. Closing a
spawned sidecar asks it to shut down, closes its pipes, and terminates its process
tree if the five-second grace period expires. Use `CloseContext` to choose that
period. `Spawn` inherits the environment; use `Connect` around your own process
when you need custom environment, working directory, or process supervision.

## Events, tools, and errors

- `OnEvent`, `OnSettings`, and `OnLog` execute in wire order on the read goroutine. Keep
  them short: queue work before making another RPC. A slow callback applies
  backpressure; recover any `stream.gap` with the generated `SessionEventsPage`.
- `HarnessEvent.Data` retains raw JSON, including future event types. Decode a
  known event into its generated payload type with `json.Unmarshal`.
- `RegisterTool(ctx, spec, handler)` installs a local callback and registers its
  schema with the host. `ToolFunc` receives `ToolCallParams` and a cancellation
  context. Return `TextResult`, `ErrorResult`, or a generated `ToolResult`.
- Context cancellation sends `$/cancelRequest` for that RPC. Cancelling the
  request that accepted a prompt does not cancel an already-running turn; call
  `SessionCancel` explicitly. Cancelling `SessionCompact` interrupts the native
  compaction operation; the session remains reserved until it stops.
- Use `errors.As(err, &rpcError)` with `var rpcError *harnesskit.Error` to inspect
  `Code`, `Reason`, and the full `Data` JSON. Unknown error details are preserved.
- `Call` is available for additive methods on newer hosts. Generated methods
  cover the current contract with typed parameters and results.

The generated `GitStatus`, `GitDiff`, `GitBranches`, `GitLog`, `GitStage`,
`GitUnstage`, `GitCommit`, `GitCheckout`, `GitFetch`, `GitPull` and `GitPush`
methods target a session without starting its runtime. Check the initialization
result's `Capabilities.Git` field and use `SettingsConfigure` for host policy.
See the [Git guide](https://github.com/tomanagle/harness-kit/blob/main/core/docs/git.md)
for examples and operation behavior.

`SessionContext(ctx, SessionIDParams{SessionID: id})` reads the latest cached
context-window observation without starting the runtime. `UsedTokens` and
`WindowTokens` are nullable pointers; check `Basis` and `Stale` before presenting
the observation as current occupancy. `SessionCompact(ctx, params)` requests
native compaction and waits for completion. Await the prior turn, check
`CanCompact`, and use the call's context or `SessionCancel` to cancel. Automatic
thresholds belong to `SettingsConfigure`'s `compaction` group. See the
[context guide](https://github.com/tomanagle/harness-kit/blob/main/core/docs/context.md).

Set `Options.OnLog` before calling the generated `LogsSubscribe(ctx, Empty{})`
method. The callback receives `LogNotification.Record` or an explicit
`LogNotification.Dropped` range; `LogsUnsubscribe` stops only this connection's
delivery. Check `Capabilities.Logs` for older hosts. Log records have structured
attributes and source locations, bounded live delivery, and no transcript replay.
Configure `logging` through the same `SettingsConfigure` method. See the
[logging guide](https://github.com/tomanagle/harness-kit/blob/main/core/docs/logging.md).

From this directory, `go test -race ./...` runs transport tests. When run inside
the repository checkout, it also builds the real sidecar and exercises an offline
fake-runtime conversation. No paid model calls are made.

## Conversation and input helpers

`Run(ctx, params)` assigns a turn ID if omitted, subscribes before sending,
recovers terminal results after stream gaps and retains no delta queue. Cancelling
its context cancels the accepted turn. Failed agent turns return a result with
`Status == "failed"`; admission, transport and replay failures return errors.
The generated `SessionPrompt` remains available for callers managing their own
turn lifecycle.

```go
conversation, err := kit.ObserveConversation(ctx, sessionID, hk.ConversationOptions{
    MaxEvents: 2000, MaxBytes: 4 * 1024 * 1024,
})
if err != nil { return err }
defer conversation.Close()
snapshot := conversation.Snapshot()
// In a UI worker: range over conversation.Updates. Slow readers receive the
// newest snapshot; intermediate view updates are coalesced.
_ = snapshot

prepared, err := hk.PrepareAttachments(ctx, "Explain this", []string{"/project/README.md"}, hk.AttachmentOptions{})
if err != nil { return err }
result, err := kit.Run(ctx, hk.SessionPromptParams{SessionID: sessionID, Input: prepared.Input})
_ = result
```

The observer subscribes before replay, uses bounded history pages and never
starts a runtime. Snapshots contain messages, tools, turn results, subtasks,
pending approvals and context. `Truncated` explicitly marks a partial window,
including when old approval or subtask state is outside it; use `ApprovalList`
for authoritative pending approvals. Defaults are 2,000 events / 4 MiB of
serialized events, not a process-memory guarantee. `Error` reports replay failures.
`NewConversationState` supplies the projection independently of a client.
`SubscribeEvents` adds independent listeners without replacing `Options.OnEvent`;
callbacks must not block or call synchronous RPCs on the read goroutine.

Attachments inline UTF-8 text and reference local PNG/JPEG/GIF/WebP images.
Defaults: 20 files, 512 KiB per text file, 5 MiB per image, 1 MiB total text and
20 MiB total images. `AttachmentOptions` accepts optional limit pointers (nil
means default, zero is explicit) and an injectable `AttachmentFiles` interface.
Image references require a shared filesystem with the runtime.

`SettingsGet(ctx, SettingsGetParams{Target: SettingsTarget{Runtime: "codex", Model: "your-model"}})`
returns discovery metadata through the unified settings API before session
creation. Field choices distinguish reported, declared and unknown sources.
Generated `CodexOptions` and `ClaudeOptions` work with `EncodeOptions` to fill
`CreateSessionParams.Options` without handwritten JSON. Custom adapters can
supply their own option structs. The generated protocol remains the source of
truth for request and response types.

## Preferences and lifecycle hooks

The sidecar's optional JSON preference store is selected through the existing
startup settings document:

```go
options := hk.Options{Settings: hk.SettingsPatch{
    "process": json.RawMessage(`{"settingsPath":"/app/preferences.json"}`),
}}
```

Pass `options` to `Spawn`. Accepted global `SettingsConfigure` calls save before
applying; later startup settings and explicit flags override saved values.
Embedded Go hosts can inject `core/settings.Store` instead; the standalone client
remains independent of the core module.

```go
err := kit.RegisterHook(ctx, hk.HookSpec{
    ID: "project-context", Name: "turn.before",
}, func(ctx context.Context, request hk.HookRequest) (hk.HookResponse, error) {
    if !request.IsFirstTurn { return hk.HookResponse{}, nil }
    input := append(request.Input, hk.InputPart{Type: "text", Text: "Project context"})
    return hk.HookResponse{Input: &input}, nil
})
if err != nil { return err }
// Later: kit.UnregisterHook(ctx, "project-context")
```

Use `SettingsConfigure`'s `hooks` group for enabled state, timeout, maximum
concurrency, failure policy and required IDs. Handlers must honor their context.
IDs belong to the registering connection; reconnecting applications register
again before prompting. After-turn handlers cannot change a completed result and
are not awaited by `Run`. See [hooks](../../core/docs/hooks.md) and
[settings persistence](../../core/docs/configuration.md#optional-preference-persistence).
