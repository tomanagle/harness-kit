# @tomanagle/harness-kit

The TypeScript client for [Harness Kit](https://github.com/tomanagle/harness-kit):
a Go library and local sidecar that gives applications one API for coding-agent
sessions, streamed events, approvals, tools, and settings.

```sh
npm install @tomanagle/harness-kit
# The published v0.2.0 sidecar uses the original Go module path.
go install github.com/tomanagle/harness-kit/cmd/harness-kit@v0.2.0
```

The npm package contains the client. The `harness-kit` sidecar and the agent CLI
you choose must be installed separately. Node.js 22.18 or newer is required for
the Node transport.

The Go SDK source lives in `core/`; clients live in the root `packages/` directory.
To build the current checkout, run `make -C core build` from the repository root; the binary is
`core/bin/harness-kit`. New Go source imports use
`github.com/tomanagle/harness-kit/core/...`; see the
[module migration notes](https://github.com/tomanagle/harness-kit/blob/main/core/docs/releases.md)
before switching from the published v0.2.0 Go module. The future coordination
server is separate from this local sidecar. Runnable applications live in the
repository-root `examples/` directory; `make example node` and
`make example electron` work from the repository root.

```ts
import { createHarnessKit } from "@tomanagle/harness-kit";
import { stdio } from "@tomanagle/harness-kit/node";

const kit = await createHarnessKit({
  transport: stdio({
    command: "harness-kit",
    settings: {
      process: { store: "./sessions", runtimes: ["codex"] },
      memory: { evictAfterIdleMs: 600_000 },
    },
  }),
});

try {
  const session = await kit.sessions.create({ runtime: "codex" });
  for await (const event of session.prompt("Explain this project")) {
    if (event.type === "message.delta") process.stdout.write(event.data.text);
  }

  // Apply live settings through the same settings object used at startup.
  await kit.settings.configure({
    patch: { memory: { evictAfterIdleMs: 300_000 } },
  });
} finally {
  await kit.close();
}
```

`@tomanagle/harness-kit` has no Node.js imports or runtime dependencies. Bring your own
`Transport` for browsers and other hosts; `@tomanagle/harness-kit/node` supplies a stdio
transport for Node and Electron's main process.

Wire types, error constants and the low-level `ProtocolClient` are generated from
the shared protocol schema. The application helpers above use those generated
methods; streaming, tool handlers and transport lifetimes remain in the client
runtime. For the exact request and response shapes, use `kit.protocol`:

```ts
const { session } = await kit.protocol.sessionCreate({ runtime: "codex" });
const page = await kit.protocol.sessionEventsPage({ sessionId: session.sessionId });
```

The current checkout also exposes `kit.sessions.context(id)` and
`kit.sessions.compact(id, { signal })`, with equivalent methods on a `Session`
handle. Context reads return the latest observation without starting a runtime;
compaction waits for native completion and supports cancellation. Await the
previous turn and check `canCompact` before requesting compaction. Nullable
`usedTokens`/`windowTokens`, `basis`, and `stale` distinguish unknown data from
current measurements. Use `context.usage` and `context.compaction` events for a
live UI, and the shared `settings.compaction` object for automatic thresholds.
These APIs require client and sidecar builds from the same current checkout;
they are not in v0.2.0. See the
[context guide](https://github.com/tomanagle/harness-kit/blob/main/core/docs/context.md).

The raw `kit.request()` method remains available for protocol extensions. In a
repository checkout, regenerate the schema and clients with `make generate`;
`make check-generated` verifies that committed output is current. The standalone
[Go client](https://github.com/tomanagle/harness-kit/tree/main/packages/client-go)
uses the same generated contract.

`await kit.logs.subscribe(listener)` enables structured Go `slog` diagnostics
and returns an async unsubscribe function. The listener receives either a
`record` (time, level, message, attributes, source) or a `dropped` marker when its
bounded stream loses records. Check `kit.server.capabilities.logs`, await
subscription readiness, and use `settings.logging` for live levels and limits.
Logs are separate from saved conversations and have no replay store. See the
[logging guide](https://github.com/tomanagle/harness-kit/blob/main/core/docs/logging.md).

Optional source-control helpers are exposed through `kit.git`: status, diffs,
branches, history, stage/unstage, commit, checkout, fetch, pull and push. They
target a session without starting its runtime. Check `kit.server.capabilities.git`
and configure host policy through `kit.settings.configure({ patch: { git: ... } })`.
See the [Git guide](https://github.com/tomanagle/harness-kit/blob/main/core/docs/git.md)
for examples, limits and application timing.

Settings describe their application timing, including live changes, next-turn
changes, and process wiring that requires a restart. See the
[configuration guide](https://github.com/tomanagle/harness-kit/blob/main/core/docs/configuration.md),
[protocol](https://github.com/tomanagle/harness-kit/blob/main/core/docs/protocol.md), and
[examples](https://github.com/tomanagle/harness-kit/tree/main/examples).

## Application helpers (current checkout)

Use result-only execution when another part of your app renders events:

```ts
const session = await kit.sessions.create({
  runtime: "codex",
  options: { sandbox: "workspace-write", approvalPolicy: "on-request" },
}); // Native options are checked against generated CodexOptions.

const conversation = session.observe({ maxEvents: 2000, maxBytes: 4 * 1024 * 1024 });
await conversation.ready;
render(conversation.getSnapshot());
const unsubscribe = conversation.subscribe(render);
const result = await session.run("Explain the project", { config: { effort: "high" } });
unsubscribe();
conversation.close();
```

`render` is your application's callback. Snapshots contain messages, tools,
subtasks, pending approvals, turn results and context usage. The observer
subscribes before replay, deduplicates events and repairs stream gaps without
waking a runtime. Snapshots own their data. Limits bound serialized event data,
not total JavaScript heap usage. `truncated` means this is a partial window;
older messages, pending approvals or child state may be outside it. Use paginated
history and `approvals.list` when a complete historical or pending-approval view
is needed. `error` reports replay failures; `refresh()` retries. Close observers
when their views unmount. The pure projection and injected `ConversationSource`
are also available from `@tomanagle/harness-kit/conversation`, including across
Electron IPC or other custom bridges.

`session.run` retains no delta queue. `session.prompt` still returns an iterable
plus `.done`, with default limits of **256 events / 1 MiB**. Set
`events: { mode: "none" }` for result-only use with a turn handle, or configure
`maxEvents`/`maxBytes` for iteration. Overflow throws `EventBufferOverflowError`
from the iterator; `.done` continues waiting for the terminal result. Read saved
history for omitted events. Breaking iteration or calling `discardEvents()`
releases queued payloads without cancelling the agent.

Build settings controls from the same settings API:

```ts
const catalog = await kit.settings.get({ runtime: "codex", model: "your-model" });
const effort = catalog.fields["session.effort"];
console.log(effort.choices, effort.allowCustom, effort.source, catalog.warnings);
await kit.settings.configure({
  target: { sessionId: session.id },
  patch: { session: { options: { approvalPolicy: "on-request" } } },
}); // Applies to future turns. Existing host permission guards still apply.
```

`reported` choices come from the installed runtime; `declared` choices come from
the adapter; `unknown` preserves uncertainty. These are not account-entitlement
checks. Read each field's scopes and application timing; custom adapters remain
extensible through raw protocol options. Your app owns presets such as “Supervised”.

Node/Electron hosts can optionally prepare file selections:

```ts
import { inspectAttachments, prepareAttachments } from "@tomanagle/harness-kit/node";
const selection = await inspectAttachments(["/project/README.md"]);
const { input } = await prepareAttachments(
  "Explain this",
  selection.map((file) => file.path),
  {
    maxTextBytes: 512 * 1024,
  },
);
await session.run(input);
```

This helper deduplicates paths, bounds reads, inlines UTF-8 text and references
local PNG/JPEG/GIF/WebP images. Defaults: 20 files, 512 KiB per text file, 5 MiB
per image, 1 MiB total text and 20 MiB total images. Every limit is configurable;
zero is explicit. Inject `files: AttachmentFiles` for your filesystem boundary,
and pass `signal` for cancellation. Image paths require a shared filesystem with
the runtime. File dialogs and attachment policies remain application decisions.

## Preferences and lifecycle hooks

Set `stdio({ command, settings: { process: { settingsPath: "/app/preferences.json" } } })`
to opt into automatic global settings persistence. Saved values load before
explicit startup overrides; accepted `kit.settings.configure()` changes save
before applying. Session-scoped settings retain their session-store lifetime.

```ts
const remove = await kit.hooks.register(
  "turn.before",
  async (context) => {
    if (!context.isFirstTurn) return;
    return { input: [...context.input, { type: "text", text: "Project context" }] };
  },
  { id: "project-context" },
);
```

The same API supports `session.beforeCreate` and `turn.after`. `context.signal`
handles cancellation; registration options include `id`, `order` and `sessionId`.
Use `settings.hooks` for enabled state, timeout, maximum concurrency, failure
policy and required handler IDs. Await registration before prompting, and await
the returned disposer to unregister. Policies can persist; functions are
registered again after restart. A completed turn does not wait for after-turn
handlers. See [hooks](../../core/docs/hooks.md) and
[settings persistence](../../core/docs/configuration.md#optional-preference-persistence)
for ordering, retries, ownership and storage guarantees.
