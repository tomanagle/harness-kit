# Structured diagnostics with slog

Harness Kit uses Go's built-in `log/slog`. Each manager exposes a structured live
stream that applications can subscribe to directly or through the sidecar.
Diagnostics are independent of durable conversation events: they have their own
sequence, no replay store, and no transcript side effects.

## TypeScript clients

Check `kit.server.capabilities.logs` when supporting older servers. Install a
listener and await subscription readiness before starting work to observe it:

```ts
const stop = await kit.logs.subscribe(({ record, dropped }) => {
  if (record) {
    appendLogRow({
      // Your UI; render message and attributes as text.
      time: record.time,
      level: record.level,
      message: record.message,
      attributes: record.attrs,
      source: record.source,
      truncated: record.truncated ?? false,
    });
  }
  if (dropped) appendLogRow({ message: `Lost ${dropped.count} log records` });
});

await session.prompt("Inspect the code").done;
await stop();
```

Multiple listeners on one client share its wire subscription. Each returned
cleanup function is independent and idempotent; closing the last listener stops
server delivery. An optional `{signal}` bounds subscription setup, not the
lifetime of an established listener. Connection close releases subscriptions.
Setup cancellation rejects promptly; any best-effort remote cleanup is bounded.

## Go hosts

Every manager has a stream, including an embedded manager with no filesystem or
sidecar. Use its logger to add your application's diagnostic records:

```go
mgr := session.NewManager(agent.NewRegistry(fake.New()), session.Options{})
defer mgr.Shutdown(context.Background())

sub := mgr.Logs().Subscribe()
defer sub.Close()
go func() {
    for notification := range sub.C {
        if notification.Record != nil {
            record := notification.Record
            fmt.Println(record.Level, record.Message, record.Attrs)
        }
        if notification.Dropped != nil {
            fmt.Println("dropped", notification.Dropped.Count)
        }
    }
}()

mgr.Logger().With("sessionId", "ses_example").WithGroup("app").Info(
    "workspace selected", "project", "demo",
)
```

The public `core/logging.Stream` implements `slog.Handler` (`Enabled`, `Handle`,
`WithAttrs`, `WithGroup`). It uses the standard JSON handler to resolve values,
preserve structured attributes/groups, and capture `slog.Record` source location
when available. Application attributes live under `attrs`, so an attribute named
`message`, `time`, or `level` cannot overwrite the record envelope.

For a stream shared by multiple host components, supply it explicitly:

```go
level := new(slog.LevelVar)
stream := logging.New(level,
    slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}),
    logging.DefaultConfig(),
)
defer stream.Close()

logger := slog.New(stream)
mgr := session.NewManager(registry, session.Options{Logs: stream})
defer mgr.Shutdown(context.Background())
// Supply logger to other components through their Logger options.
logger.Info("application ready")
```

A manager owns a stream it creates and closes it at shutdown. A stream passed in
`session.Options.Logs` is owned by the caller. `Stream.Close` releases direct
subscriptions and stops stream capture while continuing to forward records to
its optional next handler. No call changes `slog.Default` or process-global
logging configuration.

`stream.WithDestination(handler)` adds another destination while preserving the
stream's existing handler. It keeps each destination's level policy and returns
an existing handler derived from the same stream unchanged, preserving its
attributes and avoiding duplicate stream delivery.

Supplying an existing `session.Options.Logger` preserves that logger's handler
as the downstream destination. Its own `Enabled` and formatting policy remain
in force. Stream level filtering applies independently; share the `LevelVar`
with both handlers if they should follow the same threshold. A downstream
handler's formatting or redaction does not transform the separate UI stream.
Custom messages and attributes should contain only data intended for log
subscribers. Built-in session diagnostics report identifiers and lifecycle
statuses without copying prompts, tool arguments, or tool results into logs.

## Go clients

The standalone client provides generated `LogsSubscribe` and `LogsUnsubscribe`
methods plus an `OnLog` callback. Register the callback before subscribing:

```go
client, err := harnesskit.Connect(ctx, reader, writer, harnesskit.Options{
    OnLog: func(n harnesskit.LogNotification) {
        if n.Record != nil {
            fmt.Println(n.Record.Level, n.Record.Message)
        }
    },
})
if err != nil { return err }
defer client.Close()
if _, err = client.LogsSubscribe(ctx, harnesskit.Empty{}); err != nil { return err }
// Perform work. Later:
_, err = client.LogsUnsubscribe(ctx, harnesskit.Empty{})
```

Callbacks execute on the client's read goroutine. Return promptly and queue UI
work elsewhere; do not make blocking RPC calls inside the callback.
Delivery can begin before the subscribe response arrives, which is why the
callback must be installed first.

## One settings object

All log policy fields are global and live. Use the same document at startup and
`settings.configure` at runtime; changing them requires no restart:

```ts
await kit.settings.configure({
  patch: {
    logging: {
      level: "info",
      maxQueueRecords: 256,
      maxQueueBytes: 1_048_576,
      maxRecordBytes: 65_536,
    },
  },
});
```

| Field                     | Default   | Meaning                                                 |
| ------------------------- | --------- | ------------------------------------------------------- |
| `logging.level`           | `info`    | Stream threshold: `debug`, `info`, `warn`, or `error`.  |
| `logging.maxQueueRecords` | `256`     | Positive number of retained records per subscription.   |
| `logging.maxQueueBytes`   | `1048576` | Positive encoded-record byte limit per subscription.    |
| `logging.maxRecordBytes`  | `65536`   | Positive encoded size limit for each normalized record. |

Slow consumers never block the producer through the stream queue. The oldest
queued records are dropped when a bound is reached. Each subscription also has
at most one in-flight delivery and one constant-size pending loss marker.
Changing limits trims existing queues; an already in-flight delivery can still
arrive under the preceding limits. These bounds apply to retained subscription
data, not exact process RSS. Large producer messages, attributes, or `LogValuer`
results can allocate during JSON serialization before truncation. Attributes
pre-encoded by `WithAttrs` also belong to the handler, outside subscription
queues. The host controls these producer inputs; downstream handlers retain
their own allocation and blocking policies.

Oversized records first lose attributes, then source, then excess message text
at a UTF-8 boundary, with `truncated: true`. A limit too small for the record
envelope produces a dropped marker instead. Consumers should bound their own
UI history separately. The kit retains no record history when there are no
subscribers.

## Wire contract

`logs.subscribe {}` and `logs.unsubscribe {}` both return `{}`. They are
idempotent and ordered per connection. Notifications use method `log` and carry
exactly one of:

```json
{
  "record": {
    "seq": 7,
    "time": "2026-11-04T00:00:00Z",
    "level": "INFO",
    "message": "session started",
    "attrs": { "sessionId": "ses_example", "runtime": "fake" }
  }
}
```

```json
{ "dropped": { "fromSeq": 8, "toSeq": 12, "count": 5 } }
```

Records additionally support optional `source: {function, file, line}` and
`truncated`. `level` uses the standard slog text representation, including custom
numeric offsets if the host emits them. Sequence numbers increase per stream
and reset with a new stream/process. Subscribers receive future records only;
they do not receive a gap for records before subscribing. Dropped markers report
loss during that subscription, and there is no replay request to recover it.

The stdio sidecar streams the shared manager/server/tool-host diagnostics and
continues its existing human-readable stderr output. Native runtime conversation
output remains on the session event stream; diagnostics do not pretend to be a
complete capture of every runtime's internal logs.

When embedding `Server.Serve` over a custom transport, the host owns cleanup and
must unblock both read and write directions on cancellation/disconnection.
Bounded queues protect the core log producer; they cannot interrupt a blocking
`io.Writer` supplied by the host.
