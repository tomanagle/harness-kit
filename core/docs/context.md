# Context usage and compaction

Harness Kit normalizes observations of the model's current context window and
requests native compaction where the runtime supports it. Context usage is
separate from cumulative token consumption, cost accounting, and the host's
memory caches. Compacting the runtime's context never deletes the kit's saved
conversation events.

## One observation shape

`session.context` returns a `SessionContext` with `sessionId`, `state`, `usage`,
and `canCompact`. It reads cached information without starting a runtime or
making a model request. `SessionInfo.context`, `session.updated.data.context`,
and `context.usage.data` carry the same `ContextUsageData` shape:

```json
{
  "usedTokens": 48000,
  "windowTokens": 200000,
  "basis": "last_request",
  "stale": false
}
```

| Field                   | Meaning                                                                                                                           |
| ----------------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| `usedTokens`            | Latest observed window usage; `null` when unavailable. Zero is a known zero.                                                      |
| `windowTokens`          | Runtime-reported capacity; `null` when unavailable. The kit does not guess it from a model name.                                  |
| `basis: "reported"`     | The runtime reported context occupancy directly.                                                                                  |
| `basis: "last_request"` | The latest relevant model request's tokens are the best available observation. This can differ from the next request's occupancy. |
| `basis: "unknown"`      | No reliable used-token observation is available.                                                                                  |
| `stale`                 | The observation is no longer current, for example after a context-changing operation without a fresh measurement.                 |

Only display a percentage when both counts are known and the capacity is
positive. Show the basis and stale status beside the count. A fresh observation
supersedes the previous complete observation; it is not a token delta. Do not
sum these events or use `usage`/`turn.completed.usage` as context occupancy.
Stored sessions can retain a stale historical observation. Older records may
have no `context`; `session.context` still returns an explicit unknown usage.

Subscribe before reading a snapshot, and use the session event sequence for
replay and overlap deduplication. A slow cached read must not overwrite a newer
live event. Context events are saved and replayed just like message events.

```ts
let revision = 0;
const stop = kit.onEvent((event) => {
  if (event.sessionId === sessionId && event.type === "context.usage") {
    revision++;
    renderContext(event.data); // Your UI: handle null and stale explicitly.
  }
});
const beforeRead = revision;
const snapshot = await kit.sessions.context(sessionId);
if (beforeRead === revision) renderContext(snapshot.usage);
// Also available on an attached session handle: await session.context().
```

## Manual compaction

`kit.sessions.compact(id, { signal })` and `session.compact({ signal })` invoke
the runtime's native compaction and wait for completion. In the generated Go
client, use `SessionContext(ctx, SessionIDParams{SessionID: id})` and
`SessionCompact(ctx, SessionIDParams{SessionID: id})`.

Await the previous turn's completion before requesting compaction, then await
compaction before sending the next prompt. The manager admits only an idle live
session, moves it to `compacting`, and serializes native work with prompts.
Concurrent prompts/compactions fail with `session_busy`; the RPC handler does
not queue compaction behind a turn. Restore inactive sessions explicitly first.

```ts
await session.prompt("Finish this step.").done;
const context = await session.context();
if (context.canCompact) {
  const controller = new AbortController();
  // Your Cancel button can call controller.abort() or session.cancel().
  const after = await session.compact({ signal: controller.signal });
  renderContext(after.usage);
}
```

`canCompact` describes negotiated availability for this live, idle session.
The runtime's `capabilities.compaction` describes potential adapter support,
not permission to invoke the command on every session. Unsupported operations
fail with `unsupported`; the kit does not send an unrecognized slash command as
an ordinary model prompt.

Compaction can make model requests and incur token usage/cost. Reported billable
usage remains subject to the session's usage accounting and budget policy.
Abort signals send `$/cancelRequest`. `session.cancel`, session close, and host
shutdown also interrupt active compaction. Cancellation requests termination;
the session stays reserved until native work stops, so retrying immediately can
still return `session_busy`.

A runtime may also compact by itself while idle. Portable cancellation of that
background maintenance is not available; close the session to stop its runtime.
Reported usage is still accounted for, with budget enforcement at the next
admission. Host-requested manual and threshold compactions remain cancellable
and subject to the active budget policy.

## Lifecycle events

`context.compaction` includes an `operationId`, `reason` (`manual`, `threshold`,
or `native`), `status` (`started`, `completed`, `failed`, or `cancelled`),
`before`, and optional `after` and `error`. `before` and `after` use exactly the
observation shape above. Listen for this event to show progress separately from
assistant messages. Native operations can report only the lifecycle information
the runtime exposes.

Manual compaction has no user turn ID. A kit threshold operation belongs to the
accepted turn, uses that turn's ID, and runs while its state is `running`, before
the user's input reaches the runtime. A completed compaction does not imply a
fresh token count: if the runtime reports no new observation, used tokens become
unknown/stale rather than displaying a fabricated reduction.

## Unified settings

These fields live in the same startup/runtime `settings.compaction` object as
the rest of the kit's configuration:

| Field           | Default  | Meaning                                                                                                                  |
| --------------- | -------- | ------------------------------------------------------------------------------------------------------------------------ |
| `autoAtTokens`  | `null`   | Compact before the next prompt when known usage reaches this positive token count.                                       |
| `autoAtPercent` | `null`   | Compact before the next prompt at this percentage, greater than zero and at most 100; requires known usage and capacity. |
| `timeoutMs`     | `120000` | Positive timeout for a requested compaction operation.                                                                   |

Either threshold can trigger compaction. Unknown or stale observations cannot
trigger it. Both thresholds default to disabled; set a field to `null` to
disable it. These are host request thresholds, not hard context limits. The
runtime can compact during a turn independently, and disabling the kit's
thresholds does not disable the runtime's own automatic compaction.

```ts
// Default policy for sessions created afterward.
await kit.settings.configure({
  patch: {
    compaction: { autoAtPercent: 80, autoAtTokens: null, timeoutMs: 120_000 },
  },
});

// Apply to an existing session without restarting it; next operation uses it.
await kit.settings.configure({
  target: { sessionId },
  patch: {
    compaction: { autoAtPercent: 90 },
  },
});

// A single turn's override does not change the session default.
await session.prompt("Continue", {
  settings: {
    compaction: { autoAtPercent: null, autoAtTokens: 100_000 },
  },
}).done;
```

Settings metadata advertises the scopes and lifetimes for a settings UI.
Enabling thresholds for a runtime that cannot request compaction fails
explicitly; missing native negotiation can still prevent a particular session
from compacting. A failed pre-turn compaction fails the accepted turn instead
of silently sending its prompt with an unmet policy.

## Runtime differences

| Runtime     | Context observation                                                                                                                  | Manual compaction                                                                        |
| ----------- | ------------------------------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------- |
| Codex       | Latest request token count and reported model window; cumulative totals remain billing usage                                         | Native thread compaction, awaited through its completion lifecycle                       |
| Claude Code | Structured context information where emitted; otherwise latest main-conversation request tokens and reported matching model capacity | Native `/compact` command with stream-json lifecycle; requires a conversation to compact |
| ACP         | Agent-reported context usage and capacity when supported                                                                             | Only when the agent advertises the native compact command                                |
| Fake        | Unknown by default; optional fixture counts for offline testing                                                                      | Available only with context fixture options                                              |

The adapters do not infer context usage from aggregated subagent/cumulative
usage or invent capacities for unreported models. Runtime versions vary; missing
data stays explicit. For a custom runtime, emit `event.ContextUsage` and implement
`agent.Compactor` if native compaction is available.

For offline client/UI tests, create a fake session with
`options: { contextTokens: 80, contextWindowTokens: 100, compactedTokens: 20 }`.
`compactionDelayMs` can simulate a cancellable operation. These deterministic
numbers are fixture inputs, not estimates of a real model's context.

The direct-model track's [`model/contextwindow`](models.md) remains a separate,
caller-controlled mechanism for applications that own the model loop. Host
transcript eviction and process suspension are controlled by
[resource policy](configuration.md#resource-policy), not compaction.
