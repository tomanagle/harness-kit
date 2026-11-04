# Git integration

The kit provides optional source-control operations that a harness can compose
into its own workflow. The host chooses when to stage, commit, switch branches,
fetch, pull or push, and owns its UI and approval rules. The Electron and Wails
examples include a source-control panel built on these APIs. Opening it only
reads the repository; writes require explicit actions.

Git operations use a session ID, including a closed or suspended session. They
do not start its runtime. The first operation discovers the repository containing
the session's working directory and saves that binding. Dedicated worktree
sessions use their existing binding. Paths in results and requests are relative
to the repository root, even when the session starts in a subdirectory.

## TypeScript and Go

The TypeScript client provides `kit.git` helpers:

```ts
const status = await kit.git.status(conversation.id);
const diff = await kit.git.diff(conversation.id, {
  path: "src/main.ts",
  staged: false,
  maxBytes: 65536,
});
await kit.git.checkout(conversation.id, { branch: "agent/fix", create: true });
await kit.git.stage(conversation.id, ["src/main.ts"]);
const commit = await kit.git.commit(conversation.id, "Fix session restoration");
await kit.git.push(conversation.id, {
  remote: "origin",
  branch: "agent/fix",
  setUpstream: true,
});
```

Other helpers are `unstage(id, paths)`, `branches(id)`, `log(id, { ref, limit })`,
`fetch(id, { remote, branch })`, and `pull(id, { remote, branch })`. Optional
options can be omitted. Helpers accept the client's usual request options,
including cancellation. Check `kit.server.capabilities.git` before offering
these operations. Capability availability means an adapter is installed; an
individual session can still be outside a repository or have Git unavailable.

The standalone Go client generates the same methods and request types:

```go
status, err := client.GitStatus(ctx, harnesskit.SessionIDParams{SessionID: id})
_, err = client.GitStage(ctx, harnesskit.GitPathsParams{
    SessionID: id, Paths: []string{"main.go"},
})
commit, err := client.GitCommit(ctx, harnesskit.GitCommitParams{
    SessionID: id, Message: "Fix session restoration",
})
```

The shared schema defines `git.status`, `git.diff`, `git.branches`, `git.log`,
`git.stage`, `git.unstage`, `git.commit`, `git.checkout`, `git.fetch`, `git.pull`
and `git.push`. Both clients' wire methods are generated from it.

## One settings object, at startup and runtime

Supply the `git` group in the normal startup settings document, or change it
through the existing method:

```ts
await kit.settings.configure({
  patch: {
    git: {
      readOnly: false,
      allowRemote: true,
      allowDuringTurn: false,
      timeoutMs: 60000,
      maxOutputBytes: 262144,
      hooks: "inherit",
      signing: "inherit",
    },
  },
});
```

These are global host settings. Descriptions report `when: "nextOperation"`;
an accepted operation retains its captured settings, and later operations use
the new values without a restart. Session and prompt patches cannot override
host Git policy. Changing `process.gitPath` selects a different executable and
requires the normal process restart flow.

| Field             | Default     | Behavior                                                                                                         |
| ----------------- | ----------- | ---------------------------------------------------------------------------------------------------------------- |
| `readOnly`        | `false`     | Reject stage, unstage, commit, checkout and remote operations                                                    |
| `allowRemote`     | `true`      | Allow explicitly requested fetch, pull and push                                                                  |
| `allowDuringTurn` | `false`     | Reject writes while a session managed by this manager is using the same checkout; block new turns during a write |
| `timeoutMs`       | `30000`     | Deadline including repository lock waits and subprocesses                                                        |
| `maxOutputBytes`  | `1048576`   | Bound captured command output and displayed patches                                                              |
| `maxFiles`        | `10000`     | Limit listed files, branches and remotes, untracked diff files, and supplied paths                               |
| `maxLogEntries`   | `100`       | Maximum commits returned by a log request                                                                        |
| `hooks`           | `"inherit"` | Honor Git hooks, or choose `"disabled"` for these commands                                                       |
| `signing`         | `"inherit"` | Honor Git commit-signing configuration, or choose `"enabled"` / `"disabled"`                                     |

These controls govern the kit's Git API. They do not sandbox a runtime's own
shell tools or external Git processes. Native actions retain the user's Git
identity, credential helpers, SSH environment, hooks and signing configuration
unless an explicit policy overrides them. Terminal credential prompts are
disabled; credential helpers can still provide authentication. A host should
arrange authentication through its own settings and login flow.

## Operation behavior

Status reports branch/HEAD, detached or unborn state, upstream divergence,
staged and working-tree file status, conflicts, renames and configured remotes.
Credentials in URL user-info are redacted. No remote request is needed to read
status; ahead/behind counts reflect locally known refs.

Diffs show either the index or current working changes. Working diffs include
nonignored untracked files. `maxBytes` can reduce the host's output limit.
Lists and patches expose `truncated`; patches also expose `encodingLossy` when
invalid UTF-8 was replaced. Treat these as bounded display results. Saved
per-turn observations remain a separate API: see [workspaces](workspaces.md).

Stage and unstage take explicit literal paths, not Git pathspec patterns.
The repository root, traversal, metadata paths and symlink ancestors escaping
the repository are rejected. Unstage preserves working files. Commit uses only
the existing index and the caller's message; hooks retain their normal Git
behavior. No implicit stage-all step or generated commit message is imposed.

Checkout switches to an existing branch or creates a new one. Switching
branches, or creating from another commit, requires a clean checkout. Creating
a branch at the current HEAD can preserve uncommitted work. Pull requires a
clean named branch and only fast-forwards. Checkout and pull refuse to overwrite
ignored local files. The native adapter implements pull as fetch followed by
a fast-forward-only merge so this protection applies to both steps. Push uses an explicit destination,
never force-pushes, and sets upstream only when requested. Remote operations
select configured remote names; they do not accept arbitrary URLs. Omitted
remote/branch values use the branch's configured upstream, falling back to
`origin` and the current branch. Detached pushes need an explicit branch.

Operations sharing a repository are serialized, including linked worktrees and
snapshot capture. Unrelated repositories can proceed concurrently. Manager
admission guards coordinate turns and writes in the same checkout. These do
not lock out editors, other kit processes or external Git commands. A timeout
or cancellation can occur after a write has taken effect; refresh status
before retrying a mutation.

Bindings retain canonical paths and physical Git-directory identities. The
adapter rechecks them before operations, rejecting a replaced repository at
the same path. Older bindings acquire physical identities on their next use;
their historical identity cannot be reconstructed retroactively.

## Embedded adapters

The portable `workspace.SourceControl` interface is an optional extension of
`workspace.Backend`. The sidecar and native examples install the native adapter;
embedded builders choose whether to provide it:

```go
manager := session.NewManager(registry, session.Options{
    Store: store,
    Workspace: gitworkspace.New(""), // Git from PATH
})
status, err := manager.GitStatus(ctx, id)
```

Import `gitworkspace` from `github.com/tomanagle/harness-kit/core/workspace/git`.
The manager exposes matching `Git*` methods and `HasSourceControl`. A custom
backend can implement the optional interface, honoring its identity,
cancellation, output-bound and serialization contract. The portable session
and settings packages remain independent of native process execution.

GitHub/GitLab authentication, pull requests, issue links, branch naming,
automatic commits and merge workflows belong to the harness or a separate
forge adapter. They are not required to use the Git primitives.
