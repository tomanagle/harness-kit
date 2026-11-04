# Worktrees and saved turn diffs

Workspace isolation and diff capture are opt-in. They use the same settings
document and methods as models, budgets and memory policy.
For current status, staging, commits, branches and remotes, see
[Git integration](git.md). Those operations are independent of turn capture.

~~~ts
const conversation = await kit.sessions.create({
  settings: {
    session: { runtime: "codex", cwd: "/projects/example" },
    workspace: {
      mode: "worktree",
      root: "/projects/agent-worktrees",
      base: "HEAD",
      captureDiff: true,
    },
  },
});
~~~

The native backend creates a detached Git worktree under the explicit root.
Its directory name is a stable hash of the kit session ID. Alternatively, set
an absolute path that does not yet exist. Root and path are mutually exclusive.
Repo defaults to the creation working directory; base defaults to its current
HEAD. A valid commit is required, so an empty repository must have its first
commit before creating a worktree. No branch is created.

The runtime starts with the worktree as its working directory. The session
record retains its workspace identity, ownership and configuration. Suspend
and restore reuse that exact worktree; a missing or replaced worktree is an
error. A native conversation ID cannot be imported directly into a newly
created worktree because the runtime's original workspace cannot be verified.

Use mode "existing" to keep the selected session directory. Diff capture can
be enabled independently, without creating a worktree:

~~~ts
await kit.settings.configure({
  target: { sessionId: conversation.id },
  patch: { workspace: { captureDiff: true, maxDiffBytes: 131072 } },
});

const turn = conversation.prompt("Implement the change", {
  settings: { workspace: { maxSnapshotBytes: 33554432 } },
});
await turn.done;
const diff = await kit.sessions.diff(conversation.id, turn.id, { maxBytes: 65536 });
~~~

Turn is also an async iterable of events. The workspace.diff event is emitted
before turn.completed and includes the saved result, so a host can render it
without another request.

## Setting lifetimes

Global workspace settings are defaults for **new sessions**. Changing them
does not move existing sessions or change their saved capture policy.
Session-scoped captureDiff, maxDiffBytes, maxSnapshotBytes, maxFiles and
timeoutMs changes apply to the **next turn**. The same fields can be overridden
for one prompt. An active turn retains its accepted policy, recorded in
turn.started.workspace. Location fields (mode, repo, base, path, root) require
a new session.

| Field | Default | Meaning |
| --- | --- | --- |
| mode | "existing" | Use current directory or create a dedicated "worktree" |
| repo | "" | Worktree source; empty uses creation directory |
| base | "" | Commit/ref to check out; empty means HEAD |
| path | "" | Explicit new worktree path |
| root | "" | Explicit parent for a generated path |
| captureDiff | false | Capture before/after turn boundaries |
| maxDiffBytes | 262144 | Combined UTF-8 patch bytes retained per turn |
| maxSnapshotBytes | 67108864 | Working-file content bytes read per boundary |
| maxFiles | 10000 | Files per boundary/index snapshot |
| timeoutMs | 30000 | Timeout for each workspace operation |

Settings descriptions report these lifetimes for the selected scope. Hosts can
choose tighter limits. Request maxBytes can reduce the saved diff limit; it
cannot recover content omitted from the saved result. Later global defaults
do not change previously saved results. The byte limit counts patch content across
both views, excluding metadata and JSON escaping.

## What a turn diff means

The native backend snapshots immediately before calling the runtime's prompt
method and immediately after it returns, including cancellation and failures.
It compares those two snapshots:

* workingTree shows changes to tracked files and **nonignored untracked files**.
* index separately shows changes to staged content.
* before and after retain timestamps, tree IDs, HEAD, file counts and byte counts.
* complete: false and error identify failed or limited captures.
* truncated: true means the saved or requested patch exceeded its byte limit.
* encodingLossy: true means invalid UTF-8 bytes were replaced for JSON display.

Preexisting dirty content is part of the before snapshot, so it is not
mistaken for a change made during this turn. A file staged and then modified
again can produce different working-tree and index diffs. Files added, removed
or committed during the turn are compared by their boundary contents.

This is an observation of a time interval, not proof that the agent authored
every change. Concurrent edits from editors, users or other processes are
included. Snapshotting a directory is not atomic across files. A background tool
that continues after the runtime reports completion can change files outside
the captured interval. Separate worktrees reduce interference between sessions.

Snapshots cover the complete Git worktree, even when an existing session's
working directory is a subdirectory. Ignored untracked files are excluded.
Submodules, nested repositories, special files and unmerged index stages are
explicitly unsupported; capture reports an incomplete result while the agent
turn can still proceed. Symlinks are represented by their link targets.
Working-file bytes are hashed without Git clean filters or text conversion.
Diffs disable external diff programs and rename detection; renames appear as
deletion/addition. They are bounded display artifacts, not guaranteed applicable
patches when truncated or encoding-lossy.

The user's index, HEAD and working files are not changed by snapshot capture.
Temporary private indexes are used for both working-tree and staged snapshots.
Git blob/tree objects are written into the repository object database. The
bounded patches are saved in normal session event storage, after which temporary
refs/harness-kit references are released. Git's ordinary garbage collection can
later reclaim unreachable objects. Diff reads work after restart, without
Git installed, and after worktree removal when a durable event store retains
the transcript. A memory-only store has the same bounded retention as its
other transcript events.

A process crash or shutdown can leave temporary refs. Embedded hosts can call
git.Backend.ReleaseSnapshots(ctx, binding, turnID) explicitly to release one
turn's refs; this never deletes working files. Retained nonignored untracked
content may include sensitive files, so applications should choose repository
ignore rules and capture policy accordingly.

## Forking and cleanup

Forking a session that owns a worktree requires an explicit new workspace
root or path. The child gets a different worktree and ownership record.
The conversation is branched by the runtime's native fork operation. Git
contents start at the selected base commit; the parent's uncommitted files
are not silently copied. Inherited transcript diff events remain parent
observations and are not returned as child-authored turn diffs.

Session close, deletion, shutdown and failed startup never automatically
remove a worktree. If a worktree was created but runtime startup then fails,
the kit retains a failed session record containing its binding and includes
the retained path/session ID in the error. This allows inspection and explicit
cleanup.

~~~ts
await conversation.close();
await kit.sessions.removeWorkspace(conversation.id);
~~~

Removal requires the runtime to be closed or suspended, a matching ownership
marker, unchanged repository identity, and no tracked, untracked **or ignored**
changes. If HEAD advanced from the creation commit, a local branch must retain
the new HEAD; that branch remains after worktree removal. Advanced detached
commits require creating a branch before cleanup. There is no force mode.
Preserve needed work and perform exceptional cleanup through the host's own Git workflow.
Deleting the session record first does not delete its files, but also removes
the manager's saved cleanup handle.

## Embedded Go and RPC

The core workspace, settings and session packages remain dependency-free and
WASM-portable. Native Git support is injected:

~~~go
import (
    "github.com/tomanagle/harness-kit/core/session"
    gitworkspace "github.com/tomanagle/harness-kit/core/workspace/git"
)

manager := session.NewManager(registry, session.Options{
    Store: store,
    Workspace: gitworkspace.New(""), // empty uses Git from PATH
})
~~~

The sidecar and native examples provide this backend. The process.gitPath
setting (serve --git-path) chooses an executable and requires a process restart.
An embedding application can instead implement workspace.Backend with its own
storage, repository policy and workspace manager. Without a backend, workspace
creation/capture settings are rejected; saved diffs remain readable.

Go offers Manager.TurnDiff, Manager.RemoveWorkspace, and equivalent RPC client
methods. JSON-RPC v1 adds:

* session.diff — {sessionId, turnId, maxBytes?} → saved workspace.Diff.
* workspace.remove — {sessionId} → {} on verified clean removal.

Missing capture data returns reason turn_diff_not_found. Disabled capture and
an inherited parent's diff are not treated as empty successful diffs.
Initialization advertises capabilities.workspace for backend availability.

The CLI exposes the same operations:

~~~sh
harness-kit sessions diff SESSION_ID TURN_ID --max-bytes 65536
harness-kit sessions remove-workspace SESSION_ID
~~~

Session creation and fork accept the unified settings document through their
session-settings option; forks of owned worktrees must select a new path/root.

The implementation follows Git's documented
[worktree commands](https://git-scm.com/docs/git-worktree),
[private index input](https://git-scm.com/docs/git-update-index), and
[tree-to-tree diff options](https://git-scm.com/docs/git-diff). Tests create real
temporary repositories and exercise staged/untracked content, dirty baselines,
ownership, limits, forks, restoration, failure recovery and durable RPC reads.
