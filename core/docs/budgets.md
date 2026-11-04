# Usage budgets

Budgets are part of the existing settings document. There is no separate budget
configuration API. All four limits default to `null` (unlimited). Zero means no
further work is admitted. Negative values are rejected. Token limits are integers;
cost limits are nonnegative USD numbers.

```ts
await kit.settings.configure({
  patch: {
    budget: {
      maxSessionTokens: 100_000,
      maxSessionCostUsd: 5,
      maxTurnTokens: 20_000,
      maxTurnCostUsd: 1,
    },
  },
});
const session = await kit.sessions.create({ runtime: "claude" });

// Update this session immediately, including an active turn's inherited limits.
await kit.settings.configure({
  target: { sessionId: session.id },
  patch: { budget: { maxSessionCostUsd: 10 } },
});

// Override turn limits for one prompt; session-wide limits still apply.
const turn = session.prompt("Review the changes", {
  settings: { budget: { maxTurnTokens: 30_000, maxTurnCostUsd: null } },
});
```

Global budget settings supply defaults for new sessions. A session target changes
its limits live. Explicit turn overrides remain fixed for that turn; a change to
an inherited turn limit takes effect immediately. Turn input accepts only
`maxTurnTokens` and `maxTurnCostUsd`, so it cannot bypass session-wide limits.
Omitted values inherit; `null` explicitly removes a limit. Scoped `reset` restores
the session's creation-time budget. These fields appear in `settings.get` with
scope, type, lifetime, minimum and nullability descriptions for settings interfaces.

Budgets are **observed stopping thresholds**, not guaranteed provider hard caps.
The manager rejects a new turn when a limit has already been reached, and requests
cancellation when reported usage reaches an active limit. A runtime may report
usage only after inference, may delay updates, or may finish before cancellation
is processed. Already completed work keeps its actual completion status. Reaching
a threshold emits `budget.exceeded` and adds `budget` details to `turn.completed`.
An exhausted session rejects later prompts with `budget_exceeded` until its limit
is raised or cleared. The session remains available for inspection and configuration.

`SessionInfo.usage` and `session.updated.usage` report observed total tokens and
estimated cost. Missing `costUsd` means no cost estimate was supplied. Incomplete
usage is marked with `tokensComplete: false` or `costComplete: false`; its observed
total is a lower bound. Unknown cost never becomes a fabricated zero-dollar charge.
A nonzero limit is rejected as `unsupported` when the runtime advertises no
reporting for that metric (for example, Codex USD cost or ACP consumed tokens).
Session settings metadata marks those fields unavailable. Zero still denies
admission without requiring usage reporting. A runtime that supports a metric
may omit individual updates; incomplete totals expose that uncertainty. Costs come from runtime estimates, not billing records or a price table.

Usage snapshots are cumulative within their `scope` (`turn` or `session`). Repeated
snapshots and the final turn result are reconciled without counting the same spend
twice. Adapters can explicitly emit `mode: "delta"` for increments; every such event
is an additional increment. Final `TurnResult.Usage` must be a snapshot. Decreasing
session snapshots are treated as stale; adapters whose native counters restart
must normalize their epoch or report turn usage instead.

`TotalTokens` is authoritative. Claude includes separate cache reads/writes in that
total; Codex cached input is already a subset of its total. Cache and reasoning
breakdowns are never added again. Custom adapters should supply normalized
`totalTokens`; otherwise the fallback is `inputTokens + outputTokens`. A reported
zero can set `tokensKnown: true`. Context-window size and ACP context occupancy are
not billable token counters and are never used for token budgets.

Accounting checkpoints are saved with the session record. Subsequent durable events
can rebuild the checkpoint after interruption, and legacy transcripts can be
replayed without retaining their full history in memory. Closing/restoring a session
preserves usage and limits. Forks inherit the parent's observed totals and current
budget because their native conversation includes the inherited history. A first
external native resume can reveal historical cumulative usage: that total applies
to the session budget, but is not attributed entirely to the first new turn.

Fork-specific settings can change the inherited limits without changing the
parent:

```ts
const branch = await kit.sessions.fork(session.id, {
  settings: { budget: { maxSessionCostUsd: null, maxTurnCostUsd: 0.5 } },
});
```

```sh
harness-kit prompt --runtime claude \
  --session-settings '{"budget":{"maxSessionCostUsd":5}}' \
  --turn-settings '{"budget":{"maxTurnTokens":20000}}' 'Review the code'
harness-kit sessions settings ses_... '{"budget":{"maxSessionCostUsd":10}}'
harness-kit sessions settings ses_...
```

The same CLI JSON envelope also carries model, resource and workspace settings
where their scope allows. `--session-settings` applies during creation or after
restoring a session; `--turn-settings` applies to prompt input. The legacy
`--turn-config` flag cannot be combined with `--turn-settings`.
