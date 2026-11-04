package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tomanagle/harness-kit/core/workspace"
)

func gitCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, data)
	}
	return strings.TrimSpace(string(data))
}
func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}
func repository(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git not installed")
	}
	dir := t.TempDir()
	gitCommand(t, dir, "init", "-b", "main")
	write(t, filepath.Join(dir, "tracked.txt"), "base\n")
	write(t, filepath.Join(dir, ".gitignore"), "ignored.txt\n")
	gitCommand(t, dir, "add", ".")
	gitCommand(t, dir, "-c", "user.name=Harness Test", "-c", "user.email=test@example.invalid", "commit", "-m", "initial")
	return dir
}

func TestBoundarySnapshotsIncludeStagedAndUntrackedWithoutChangingIndex(t *testing.T) {
	ctx := context.Background()
	repo := repository(t)
	backend := New("")
	policy := workspace.Defaults()
	policy.CaptureDiff = true
	binding, err := backend.Prepare(ctx, "session", repo, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo, "tracked.txt"), "already staged\n")
	gitCommand(t, repo, "add", "tracked.txt")
	write(t, filepath.Join(repo, "tracked.txt"), "before turn\n")
	write(t, filepath.Join(repo, "draft.txt"), "draft before\n")
	write(t, filepath.Join(repo, "ignored.txt"), "secret ignored content\n")
	indexBefore, err := os.ReadFile(filepath.Join(binding.GitDir, "index"))
	if err != nil {
		t.Fatal(err)
	}
	head := gitCommand(t, repo, "rev-parse", "HEAD")
	before, err := backend.Capture(ctx, binding, "turn", "before", policy)
	if err != nil {
		t.Fatal(err)
	}
	indexAfter, _ := os.ReadFile(filepath.Join(binding.GitDir, "index"))
	if string(indexBefore) != string(indexAfter) {
		t.Fatal("snapshot modified user index")
	}
	write(t, filepath.Join(repo, "tracked.txt"), "staged after\n")
	gitCommand(t, repo, "add", "tracked.txt")
	write(t, filepath.Join(repo, "tracked.txt"), "unstaged after\n")
	write(t, filepath.Join(repo, "draft.txt"), "draft after\n")
	newFile := "new file.txt"
	if runtime.GOOS != "windows" {
		// Windows forbids control characters in filenames. Unix also exercises
		// the NUL-delimited Git path handling with newlines and tabs.
		newFile = "new file\nwith tab\t.txt"
	}
	write(t, filepath.Join(repo, newFile), "new content\n")
	after, err := backend.Capture(ctx, binding, "turn", "after", policy)
	if err != nil {
		t.Fatal(err)
	}
	change := workspace.TurnChange{SessionID: "session", TurnID: "turn", Binding: binding, Before: &before, After: &after, Complete: true, MaxDiffBytes: policy.MaxDiffBytes}
	diff, err := backend.Diff(ctx, change, workspace.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"-before turn", "+unstaged after", "-draft before", "+draft after", "+new content"} {
		if !strings.Contains(diff.WorkingTree, text) {
			t.Fatalf("missing %q: %s", text, diff.WorkingTree)
		}
	}
	if strings.Contains(diff.WorkingTree, "secret ignored") || strings.Contains(diff.WorkingTree, "-base") {
		t.Fatal("diff is cumulative or includes ignored content")
	}
	if !strings.Contains(diff.Index, "-already staged") || !strings.Contains(diff.Index, "+staged after") {
		t.Fatal(diff.Index)
	}
	if gitCommand(t, repo, "rev-parse", "HEAD") != head {
		t.Fatal("snapshot changed HEAD")
	}
	bounded, err := backend.Diff(ctx, change, workspace.DiffOptions{MaxBytes: 17})
	if err != nil {
		t.Fatal(err)
	}
	if !bounded.Truncated || len(bounded.Index)+len(bounded.WorkingTree) > 17 {
		t.Fatalf("unbounded result: %+v", bounded)
	}
	if refs := gitCommand(t, repo, "for-each-ref", "--format=%(refname)", "refs/harness-kit"); refs == "" {
		t.Fatal("temporary refs missing")
	}
	if err = backend.ReleaseSnapshots(ctx, binding, "turn"); err != nil {
		t.Fatal(err)
	}
	if refs := gitCommand(t, repo, "for-each-ref", "--format=%(refname)", "refs/harness-kit"); refs != "" {
		t.Fatal("temporary refs leaked: " + refs)
	}
}

func TestOwnedWorktreeRestoreAndExplicitCleanRemoval(t *testing.T) {
	ctx := context.Background()
	repo := repository(t)
	backend := New("")
	policy := workspace.Defaults()
	policy.Mode = "worktree"
	policy.Root = filepath.Join(t.TempDir(), "worktrees")
	binding, err := backend.Prepare(ctx, "one", repo, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !binding.Owned || binding.Path == repo || binding.BaseCommit == "" {
		t.Fatalf("invalid binding: %+v", binding)
	}
	if branch := gitCommand(t, binding.Path, "rev-parse", "--abbrev-ref", "HEAD"); branch != "HEAD" {
		t.Fatal("worktree was not detached")
	}
	if got, err := backend.Prepare(ctx, "one", repo, policy, &binding); err != nil || got != binding {
		t.Fatalf("restore: %+v %v", got, err)
	}
	if _, err = backend.Prepare(ctx, "different", repo, policy, &binding); !errors.Is(err, workspace.ErrNotOwned) {
		t.Fatal(err)
	}
	write(t, filepath.Join(binding.Path, "ignored.txt"), "keep me")
	if err = backend.Remove(ctx, binding); !errors.Is(err, workspace.ErrDirty) {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(binding.Path, "ignored.txt")); err != nil {
		t.Fatal(err)
	}
	if err = backend.Remove(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(binding.Path); !os.IsNotExist(err) {
		t.Fatal("worktree still exists")
	}
	if _, err = backend.Prepare(ctx, "one", repo, policy, &binding); err == nil {
		t.Fatal("missing worktree silently recreated")
	}
	if _, err = os.Stat(filepath.Join(repo, "tracked.txt")); err != nil {
		t.Fatal("main worktree changed")
	}
}

func TestWorkspaceOwnershipAndSnapshotLimits(t *testing.T) {
	ctx := context.Background()
	repo := repository(t)
	backend := New("")
	policy := workspace.Defaults()
	binding, err := backend.Prepare(ctx, "session", repo, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = backend.Remove(ctx, binding); !errors.Is(err, workspace.ErrNotOwned) {
		t.Fatal(err)
	}
	limited := policy
	limited.MaxSnapshotBytes = 1
	if _, err = backend.Capture(ctx, binding, "limit", "before", limited); !errors.Is(err, workspace.ErrLimit) {
		t.Fatal(err)
	}
	limited = policy
	limited.MaxFiles = 1
	if _, err = backend.Capture(ctx, binding, "limit", "before", limited); !errors.Is(err, workspace.ErrLimit) {
		t.Fatal(err)
	}
	policy.Mode = "worktree"
	policy.Path = repo
	if _, err = backend.Prepare(ctx, "no-adoption", repo, policy, nil); !errors.Is(err, workspace.ErrInvalid) {
		t.Fatal(err)
	}
	policy.Path = filepath.Join(t.TempDir(), "owned")
	owned, err := backend.Prepare(ctx, "owned", repo, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	write(t, ownerPath(owned), `{"sessionId":"somebody-else"}`)
	if err = backend.Remove(ctx, owned); !errors.Is(err, workspace.ErrNotOwned) {
		t.Fatal(err)
	}
}

func TestSnapshotsRejectSubmodulesAndUnmergedIndex(t *testing.T) {
	ctx := context.Background()
	repo := repository(t)
	backend := New("")
	policy := workspace.Defaults()
	binding, err := backend.Prepare(ctx, "session", repo, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(repo, "submodule"), 0700); err != nil {
		t.Fatal(err)
	}
	head := gitCommand(t, repo, "rev-parse", "HEAD")
	gitCommand(t, repo, "update-index", "--add", "--cacheinfo", "160000,"+head+",submodule")
	if _, err = backend.Capture(ctx, binding, "submodule", "before", policy); !errors.Is(err, workspace.ErrUnsupported) {
		t.Fatal(err)
	}
	other := repository(t)
	otherBinding, err := backend.Prepare(ctx, "unmerged", other, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	blob := gitCommand(t, other, "rev-parse", "HEAD:tracked.txt")
	cmd := exec.Command("git", "-C", other, "update-index", "--index-info")
	cmd.Stdin = strings.NewReader("0 " + strings.Repeat("0", 40) + "\ttracked.txt\n" +
		"100644 " + blob + " 1\ttracked.txt\n100644 " + blob + " 2\ttracked.txt\n100644 " + blob + " 3\ttracked.txt\n")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unmerged fixture: %v %s", err, output)
	}
	if _, err = backend.Capture(ctx, otherBinding, "unmerged", "before", policy); !errors.Is(err, workspace.ErrUnsupported) {
		t.Fatal(err)
	}
}

func TestCleanupPreservesDetachedCommits(t *testing.T) {
	ctx := context.Background()
	repo := repository(t)
	backend := New("")
	policy := workspace.Defaults()
	policy.Mode = "worktree"
	policy.Root = t.TempDir()
	binding, err := backend.Prepare(ctx, "commit", repo, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(binding.Path, "tracked.txt"), "valuable committed change\n")
	gitCommand(t, binding.Path, "add", "tracked.txt")
	gitCommand(t, binding.Path, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "valuable")
	if err = backend.Remove(ctx, binding); !errors.Is(err, workspace.ErrDirty) {
		t.Fatal(err)
	}
	if _, err = os.Stat(binding.Path); err != nil {
		t.Fatal("detached commit worktree removed")
	}
}
