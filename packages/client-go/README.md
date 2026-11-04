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
    events := make(chan hk.HarnessEvent, 128)
    kit, err := hk.Spawn(ctx, "./core/bin/harness-kit",
        []string{"serve", "--fake-runtime"}, nil, hk.Options{
            Settings: hk.SettingsPatch{
                "memory": json.RawMessage(`{"evictAfterIdleMs":600000}`),
            },
            OnEvent: func(event hk.HarnessEvent) { events <- event },
        })
    if err != nil { log.Fatal(err) }
    defer kit.Close()

    created, err := kit.SessionCreate(ctx, hk.CreateSessionParams{Runtime: "fake"})
    if err != nil { log.Fatal(err) }
    // Choose the turn ID before sending: events can precede the RPC response.
    const turnID = "my-first-turn"
    _, err = kit.SessionPrompt(ctx, hk.SessionPromptParams{
        SessionID: created.Session.ID,
        TurnID: turnID,
        Input: []hk.InputPart{{Type: "text", Text: "Hello"}},
    })
    if err != nil { log.Fatal(err) }
    for {
        select {
        case event := <-events:
            if event.TurnID != turnID { continue }
            if event.Type == "message.delta" {
                var delta hk.MessageDeltaData
                if err := json.Unmarshal(event.Data, &delta); err != nil { log.Fatal(err) }
                fmt.Print(delta.Text)
            }
            if event.Type == "turn.completed" { return }
        case <-kit.Done():
            log.Fatalf("connection ended: %v", kit.Err())
        }
    }
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

- `OnEvent` and `OnSettings` execute in wire order on the read goroutine. Keep
  them short: queue work before making another RPC. A slow callback applies
  backpressure; recover any `stream.gap` with the generated `SessionEventsPage`.
- `HarnessEvent.Data` retains raw JSON, including future event types. Decode a
  known event into its generated payload type with `json.Unmarshal`.
- `RegisterTool(ctx, spec, handler)` installs a local callback and registers its
  schema with the host. `ToolFunc` receives `ToolCallParams` and a cancellation
  context. Return `TextResult`, `ErrorResult`, or a generated `ToolResult`.
- Context cancellation sends `$/cancelRequest` for that RPC. Cancelling the
  request that accepted a prompt does not cancel an already-running turn; call
  `SessionCancel` explicitly.
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

From this directory, `go test -race ./...` runs transport tests. When run inside
the repository checkout, it also builds the real sidecar and exercises an offline
fake-runtime conversation. No paid model calls are made.
