# Live runtime validation

Default tests use fake CLIs and HTTP fixtures. Live tests use the already-installed,
already-signed-in CLIs and consume quota; they never open credential files or change
authentication. No API key is required when the CLI supports subscription login.

The basic smoke tests run two short prompts per runtime:

```sh
make test-live
```

For persistence, resume, fork, settings inheritance, cancellation and process
cleanup, explicitly enable the lifecycle suite. Select the models available to
your account, including a distinct override model if you want to verify switching:

```sh
HARNESS_KIT_LIVE=1 HARNESS_KIT_LIVE_LIFECYCLE=1 \
HARNESS_KIT_LIVE_CODEX_MODEL="$CODEX_MODEL" \
HARNESS_KIT_LIVE_CODEX_OVERRIDE_MODEL="$CODEX_OVERRIDE_MODEL" \
HARNESS_KIT_LIVE_CLAUDE_MODEL="$CLAUDE_MODEL" \
HARNESS_KIT_LIVE_CLAUDE_OVERRIDE_MODEL="$CLAUDE_OVERRIDE_MODEL" \
go test -count=1 -v -run TestLiveLifecycle ./runtimes/integration
```

Filter one runtime with `-run 'TestLiveLifecycle/claude'` or
`-run 'TestLiveLifecycle/codex'`. Without model variables, Codex inherits its native
model and Claude uses the smoke-test default `haiku`. Without an override model,
the suite checks effort override/inheritance and logs that distinct model switching
was not selected. Models must support `low` and `medium` effort.

Each runtime completes four short prompts and starts one interrupted prompt. The
suite checks persisted typed events with a two-event memory cache, reopens the
manager, resumes the same native ID, branches native history into a different ID,
and verifies that a child prompt does not change the parent's history. Claude also
receives a generated red PNG through stream-json image input. Model/effort patches
use the same settings envelope as production clients. Claude's observed model is
checked against its init event as well as the accepted settings snapshot.

Workspaces and kit stores are temporary directories. Codex uses a read-only
sandbox with approvals disabled; Claude has no enabled tools. Native CLI session
transcripts remain in each CLI's own store for debugging, and native user-level
configuration can still load. The tests do not enable Claude `--bare`, since that
mode does not use Claude.ai subscription login. Process-exit checks observe only
the test process's descendants and are available where `ps` is installed (macOS
and Linux). Tests log unavailable authentication or process-observation coverage;
a skipped test is not evidence of a successful live run.

## Claude cost accounting

The [official cost documentation](https://code.claude.com/docs/en/agent-sdk/cost-tracking)
distinguishes versions before and after 2.1.277. Older resumed print invocations
report their own spend. Newer invocations restore prior session totals, including
when forking, so the adapter emits a session cost snapshot and derives a turn delta
only when its baseline is known. A fresh fork's inherited spend is never labelled
as new turn cost. The first resumed/forked turn may have unknown turn cost while
still reporting a cumulative session estimate.

The [CLI budget flag](https://code.claude.com/docs/en/cli-reference) counts only
the invocation's new spend, independently of restored totals in `total_cost_usd`.
The lifecycle suite logs observed cumulative costs across resumed invocations;
offline tests cover old/new version semantics and unknown fork baselines. Reported
dollar values are the CLI's estimates, not authoritative billing amounts.

## Recorded validation

On 2026-11-04, the lifecycle suite passed with Codex 0.166.0 and Claude Code
2.1.317 using existing subscription authentication. Model switching was exercised
with `gpt-6-luna` → `gpt-6.1-sol` → `gpt-6-luna` and `haiku` → `sonnet` → `haiku`;
effort changed from low to medium and back to low. Both runtimes passed persisted
replay, native resume/fork independence, cancellation and observed process exit.
Claude passed stream-json image input as well.

Claude reported cumulative native costs of $0.00065588 on the initial invocation,
$0.01414568 after resume, and $0.01434726 after the next parent invocation. Its
independent fork reported $0.01435026. These observations support the documented
restored-total behavior; they are specific test results, not a performance or
pricing guarantee. Native MCP loading was disabled for these runs.
