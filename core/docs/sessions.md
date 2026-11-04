# Saved conversations and catalog

A `FileStore` keeps the session record and its ordered `.events.jsonl` transcript
next to each other. History APIs can replay them without starting a runtime.
Use `session.eventsPage` / `sessions.eventsPage` to bound application memory;
`events` is a convenience that collects pages. An in-memory store retains only
the configured replay window.

Catalog operations are data mutations; model, process and resource settings
remain in the shared `settings.get` / `settings.configure` object.

```ts
await kit.sessions.setTags(id, ["work", "review"]);
await kit.sessions.archive(id);
const sessions = await kit.sessions.list({
  query: "authentication", searchTranscript: true,
  tags: ["work"], archived: false, limit: 20,
});
await kit.sessions.archive(id, false);
```

Omitted `archived` includes all sessions, preserving the original list behavior.
Archiving never closes a runtime, cancels work or deletes history. Tags are
lowercased, whitespace-normalized, sorted and deduplicated; their canonical JSON
array is stored in `metadata.tags`. Setting an empty list removes that key and
preserves unrelated metadata. Each mutation publishes `session.updated`, including
`metadata`, `archived` and `forkPending`, so connected clients can update directly.

Queries match case-insensitive title substrings. `searchTranscript: true` also
searches saved event content, including adjacent text deltas from a message.
All requested tags must match. Runtime, cwd and limit are optional filters.
Transcript searches stream events with bounded memory; they are linear scans,
not an index. Custom stores without `EventScanner` use bounded pages. No runtime
needs to be resident. Hosts choose which filters their UI applies by default.

## Branching

```ts
const branch = await kit.sessions.fork(id, {
  title: "Alternative approach",
  settings: { session: { model: "your-model" } },
});
```

The Go equivalent is `Manager.Fork(ctx, id, session.ForkParams{...})`. Fork inherits
current session settings and metadata, with optional overrides in the same settings
envelope used at creation. Runtime and resume ID cannot be overridden. A fork is
unarchived and unpinned unless `settings.memory.pinned` is requested. It has its own
kit ID, runtime conversation, independent event sequence and durable lineage:
`forkedFrom`, `forkedAtSeq`, `inheritedTurns`. Inherited events keep their original
sequence numbers, timestamps and turn IDs with the child's session ID. The next
event follows the copied cursor. The parent and its transcript remain unchanged.
`turns` includes the inherited turns. FileStore copies history to an atomically
installed JSONL file with bounded memory; custom EventStores may implement
`EventCopier` to do the same. Non-durable hosts can copy only their retained window.

Forking requires a completed native conversation and an idle source. The portable
operation branches the whole conversation; arbitrary turn cutoffs are deliberately
not accepted. Unknown request fields are rejected. Adapters advertise `fork` and
optionally `forkDeferred`, and implement the optional `agent.Forker` interface.

Codex uses `thread/fork` and returns a new native thread ID without running a model.
Claude Code uses `--resume <source> --fork-session` on the first real prompt. Until
that prompt materializes the branch, `forkPending` is true and `runtimeSessionId`
is absent. Pending state survives closing and restoring the kit session. If its
source receives another turn first, the pending child's prompt fails with
`fork_source_changed`; create a fresh fork. While that child is materializing,
its source cannot accept another turn. This prevents silently using native history
that differs from the copied transcript. External mutation of native transcripts
is outside the kit's single-writer contract.

```sh
harness-kit sessions list --query auth --search-transcript --tags work --archived false
harness-kit sessions archive ses_...
harness-kit sessions unarchive ses_...
harness-kit sessions tags ses_... work review
harness-kit sessions fork ses_... --title 'Alternative approach'
harness-kit sessions fork ses_... --session-settings '{"budget":{"maxTurnTokens":20000}}'
harness-kit prompt --resume ses_... --image ./screenshot.png 'Explain this screen'
```

`--image` may be repeated; chat attaches these startup images to its first prompt.
Runtime capabilities report whether image input is accepted. Image paths are saved
as prompt references; the kit does not copy image files into the transcript store.
