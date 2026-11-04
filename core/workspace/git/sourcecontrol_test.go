package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tomanagle/harness-kit/core/workspace"
)

func sourceFixture(t *testing.T) (*Backend, workspace.Binding, workspace.GitSettings) {
	t.Helper()
	repo := repository(t)
	gitCommand(t, repo, "config", "user.name", "Harness Test")
	gitCommand(t, repo, "config", "user.email", "test@example.invalid")
	gitCommand(t, repo, "config", "commit.gpgSign", "false")
	b := New("")
	binding, err := b.Resolve(context.Background(), "source-test", repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b, binding, workspace.GitDefaults()
}

func TestSourceControlWorkflow(t *testing.T) {
	b, binding, policy := sourceFixture(t)
	ctx := context.Background()
	name := "new space.txt"
	if runtime.GOOS != "windows" {
		name = "new file\nwith tab\t.txt"
	}
	write(t, filepath.Join(binding.Path, name), "new content\n")
	write(t, filepath.Join(binding.Path, "tracked.txt"), "updated content\n")
	gitCommand(t, binding.Path, "remote", "add", "origin", "https://user:secret@example.invalid/repo.git")
	status, err := b.Status(ctx, binding, policy)
	if err != nil {
		t.Fatal(err)
	}
	if status.Branch != "main" || len(status.Files) != 2 || status.Head == "" {
		t.Fatalf("status: %+v", status)
	}
	if len(status.Remotes) != 1 || strings.Contains(status.Remotes[0].FetchURL, "secret") || strings.Contains(status.Remotes[0].FetchURL, "user@") {
		t.Fatalf("remote credentials exposed: %+v", status.Remotes)
	}
	diff, err := b.WorkingDiff(ctx, binding, workspace.FileDiffParams{}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff.Patch, "+new content") || !strings.Contains(diff.Patch, "+updated content") {
		t.Fatalf("working diff: %+v", diff)
	}
	if err := b.Stage(ctx, binding, workspace.PathsParams{Paths: []string{name}}, policy); err != nil {
		t.Fatal(err)
	}
	staged, err := b.WorkingDiff(ctx, binding, workspace.FileDiffParams{Staged: true}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(staged.Patch, "+new content") || strings.Contains(staged.Patch, "updated content") {
		t.Fatalf("staged diff: %s", staged.Patch)
	}
	if err := b.Unstage(ctx, binding, workspace.PathsParams{Paths: []string{name}}, policy); err != nil {
		t.Fatal(err)
	}
	if gitCommand(t, binding.Path, "diff", "--cached", "--name-only") != "" {
		t.Fatal("unstage left requested new file staged")
	}
	if err := b.Checkout(ctx, binding, workspace.CheckoutParams{Branch: "feature/work", Create: true}, policy); err != nil {
		t.Fatal(err)
	}
	if err := b.Stage(ctx, binding, workspace.PathsParams{Paths: []string{name}}, policy); err != nil {
		t.Fatal(err)
	}
	result, err := b.Commit(ctx, binding, workspace.CommitParams{Message: "Add new file\n\nKeep tracked edit outside this commit."}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if result.Commit == "" || gitCommand(t, binding.Path, "show", "HEAD:tracked.txt") != "base" {
		t.Fatal("commit included an unstaged file")
	}
	branches, err := b.Branches(ctx, binding, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(branches.Branches) != 2 {
		t.Fatalf("branches: %+v", branches)
	}
	history, err := b.Log(ctx, binding, workspace.LogParams{Limit: 1}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Commits) != 1 || history.Commits[0].ID != result.Commit || !history.Truncated {
		t.Fatalf("history: %+v", history)
	}
	if err := b.Checkout(ctx, binding, workspace.CheckoutParams{Branch: "main"}, policy); !errors.Is(err, workspace.ErrDirty) {
		t.Fatalf("dirty branch switch: %v", err)
	}
}

func TestSourceControlUnbornRenameAndBounds(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	gitCommand(t, repo, "init", "-b", "main")
	b := New("")
	binding, err := b.Resolve(ctx, "empty", repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	policy := workspace.GitDefaults()
	status, err := b.Status(ctx, binding, policy)
	if err != nil || !status.Unborn || status.Head != "" || status.Branch != "main" {
		t.Fatalf("unborn: %+v %v", status, err)
	}
	write(t, filepath.Join(repo, "first.txt"), "first\n")
	if err := b.Stage(ctx, binding, workspace.PathsParams{Paths: []string{"first.txt"}}, policy); err != nil {
		t.Fatal(err)
	}
	if err := b.Unstage(ctx, binding, workspace.PathsParams{Paths: []string{"first.txt"}}, policy); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "first.txt")); err != nil {
		t.Fatal("unstage removed working file")
	}
	b, binding, policy = sourceFixture(t)
	gitCommand(t, binding.Path, "mv", "tracked.txt", "renamed.txt")
	status, err = b.Status(ctx, binding, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Files) != 1 || status.Files[0].Path != "renamed.txt" || status.Files[0].OriginalPath != "tracked.txt" {
		t.Fatalf("rename: %+v", status.Files)
	}
	write(t, filepath.Join(binding.Path, "long.txt"), strings.Repeat("🤖", 1000))
	diff, err := b.WorkingDiff(ctx, binding, workspace.FileDiffParams{Path: "long.txt", MaxBytes: 127}, policy)
	if err != nil || !diff.Truncated || len(diff.Patch) > 127 || !utf8.ValidString(diff.Patch) {
		t.Fatalf("bounded diff: %+v %v", diff, err)
	}
	policy.MaxFiles = 1
	write(t, filepath.Join(binding.Path, "another.txt"), "more\n")
	status, err = b.Status(ctx, binding, policy)
	if err != nil || len(status.Files) != 1 || !status.Truncated {
		t.Fatalf("bounded status: %+v %v", status, err)
	}
}

func TestLiteralPathsAndPolicy(t *testing.T) {
	b, binding, policy := sourceFixture(t)
	ctx := context.Background()
	for _, path := range []string{"../outside", filepath.Join(binding.Path, "tracked.txt"), ".git/config", "a/../tracked.txt"} {
		if err := b.Stage(ctx, binding, workspace.PathsParams{Paths: []string{path}}, policy); !errors.Is(err, workspace.ErrInvalid) {
			t.Errorf("unsafe path %q: %v", path, err)
		}
	}
	if runtime.GOOS != "windows" {
		write(t, filepath.Join(binding.Path, "*.txt"), "literal star\n")
		write(t, filepath.Join(binding.Path, "other.txt"), "not selected\n")
		if err := b.Stage(ctx, binding, workspace.PathsParams{Paths: []string{"*.txt"}}, policy); err != nil {
			t.Fatal(err)
		}
		if got := gitCommand(t, binding.Path, "diff", "--cached", "--name-only"); got != "*.txt" {
			t.Fatalf("path interpreted as glob: %s", got)
		}
		outside := t.TempDir()
		write(t, filepath.Join(outside, "secret.txt"), "outside")
		if err := os.Symlink(outside, filepath.Join(binding.Path, "escape")); err != nil {
			t.Fatal(err)
		}
		if err := b.Stage(ctx, binding, workspace.PathsParams{Paths: []string{"escape/secret.txt"}}, policy); !errors.Is(err, workspace.ErrInvalid) {
			t.Fatalf("symlink escape: %v", err)
		}
		if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(binding.Path, "file-link")); err != nil {
			t.Fatal(err)
		}
		diff, err := b.WorkingDiff(ctx, binding, workspace.FileDiffParams{Path: "file-link"}, policy)
		if err != nil || strings.Contains(diff.Patch, "+outside") {
			t.Fatalf("symlink diff followed file contents: %s %v", diff.Patch, err)
		}
	}
	policy.ReadOnly = true
	if err := b.Stage(ctx, binding, workspace.PathsParams{Paths: []string{"tracked.txt"}}, policy); !errors.Is(err, workspace.ErrReadOnly) {
		t.Fatalf("read-only: %v", err)
	}
	policy.ReadOnly = false
	policy.AllowRemote = false
	if _, err := b.Fetch(ctx, binding, workspace.RemoteParams{}, policy); !errors.Is(err, workspace.ErrRemoteDisabled) {
		t.Fatalf("disabled network: %v", err)
	}
	other := binding
	other.GitDir = "wrong"
	if _, err := b.Status(ctx, other, policy); !errors.Is(err, workspace.ErrNotOwned) {
		t.Fatalf("replaced repository identity: %v", err)
	}
}

func TestRemoteOperationsAndBranchRetention(t *testing.T) {
	b, binding, policy := sourceFixture(t)
	ctx := context.Background()
	bare := t.TempDir()
	gitCommand(t, bare, "init", "--bare", "-b", "main")
	gitCommand(t, binding.Path, "remote", "add", "origin", bare)
	if _, err := b.Push(ctx, binding, workspace.RemoteParams{SetUpstream: true}, policy); err != nil {
		t.Fatal(err)
	}
	second := t.TempDir()
	gitCommand(t, second, "clone", bare, "checkout")
	second = filepath.Join(second, "checkout")
	gitCommand(t, second, "config", "user.name", "Peer")
	gitCommand(t, second, "config", "user.email", "peer@example.invalid")
	write(t, filepath.Join(second, "peer.txt"), "remote\n")
	gitCommand(t, second, "add", "peer.txt")
	gitCommand(t, second, "-c", "commit.gpgSign=false", "commit", "-m", "remote change")
	gitCommand(t, second, "push", "origin", "main")
	if _, err := b.Fetch(ctx, binding, workspace.RemoteParams{}, policy); err != nil {
		t.Fatal(err)
	}
	status, err := b.Status(ctx, binding, policy)
	if err != nil || status.Behind != 1 {
		t.Fatalf("upstream: %+v %v", status, err)
	}
	if _, err := b.Pull(ctx, binding, workspace.RemoteParams{}, policy); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(binding.Path, "peer.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Push(ctx, binding, workspace.RemoteParams{Remote: bare}, policy); !errors.Is(err, workspace.ErrInvalid) {
		t.Fatalf("arbitrary remote URL accepted: %v", err)
	}
	settings := workspace.Defaults()
	settings.Mode = "worktree"
	settings.Root = t.TempDir()
	worktree, err := b.Prepare(ctx, "owned-source", binding.Path, settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Checkout(ctx, worktree, workspace.CheckoutParams{Create: true, Branch: "retained-work"}, policy); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(worktree.Path, "owned.txt"), "retain\n")
	if err := b.Stage(ctx, worktree, workspace.PathsParams{Paths: []string{"owned.txt"}}, policy); err != nil {
		t.Fatal(err)
	}
	commit, err := b.Commit(ctx, worktree, workspace.CommitParams{Message: "retained worktree commit"}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Remove(ctx, worktree); err != nil {
		t.Fatal(err)
	}
	if got := gitCommand(t, binding.Path, "rev-parse", "refs/heads/retained-work"); got != commit.Commit {
		t.Fatalf("worktree cleanup lost branch: %s", got)
	}
}

func TestRepositoryLocksCancelAndDoNotBlockOtherRepos(t *testing.T) {
	b, binding, policy := sourceFixture(t)
	_, other, _ := sourceFixture(t)
	unlock, err := b.lockRepository(context.Background(), binding.CommonDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := b.Status(ctx, binding, policy); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued lock did not cancel: %v", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	if _, err := b.Status(ctx2, other, policy); err != nil {
		t.Fatalf("unrelated repository blocked: %v", err)
	}
	if _, err := b.Resolve(ctx2, "source-test", binding.Path, &binding); err != nil {
		t.Fatalf("read-only resolution waited for mutation: %v", err)
	}
}

func TestCommitHookPolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook fixture")
	}
	b, binding, policy := sourceFixture(t)
	ctx := context.Background()
	hook := filepath.Join(binding.CommonDir, "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(binding.Path, "tracked.txt"), "new\n")
	if err := b.Stage(ctx, binding, workspace.PathsParams{Paths: []string{"tracked.txt"}}, policy); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Commit(ctx, binding, workspace.CommitParams{Message: "blocked"}, policy); err == nil {
		t.Fatal("inherited hook was bypassed")
	}
	policy.Hooks = "disabled"
	if _, err := b.Commit(ctx, binding, workspace.CommitParams{Message: "explicitly bypass hooks"}, policy); err != nil {
		t.Fatal(err)
	}
}

func TestConflictStatusAndNonFastForwardPull(t *testing.T) {
	b, binding, policy := sourceFixture(t)
	ctx := context.Background()
	gitCommand(t, binding.Path, "checkout", "-b", "conflict")
	write(t, filepath.Join(binding.Path, "tracked.txt"), "branch version\n")
	gitCommand(t, binding.Path, "commit", "-am", "branch version")
	gitCommand(t, binding.Path, "checkout", "main")
	write(t, filepath.Join(binding.Path, "tracked.txt"), "main version\n")
	gitCommand(t, binding.Path, "commit", "-am", "main version")
	_, _, _, mergeErr := b.actionRun(ctx, binding.Path, policy, nil, policy.MaxOutputBytes, "merge", "conflict")
	if mergeErr == nil {
		t.Fatal("fixture merge should conflict")
	}
	status, err := b.Status(ctx, binding, policy)
	if err != nil || len(status.Files) != 1 || !status.Files[0].Conflicted {
		t.Fatalf("conflict status: %+v %v", status, err)
	}
	gitCommand(t, binding.Path, "merge", "--abort")
	bare := t.TempDir()
	gitCommand(t, bare, "init", "--bare", "-b", "main")
	gitCommand(t, binding.Path, "remote", "add", "origin", bare)
	if _, err := b.Push(ctx, binding, workspace.RemoteParams{Branch: "main", SetUpstream: true}, policy); err != nil {
		t.Fatal(err)
	}
	// Point the remote at the divergent conflict branch, then prove pull cannot
	// create a merge/rebase or move this checkout away from its existing HEAD.
	gitCommand(t, binding.Path, "push", "origin", "conflict:other")
	head := gitCommand(t, binding.Path, "rev-parse", "HEAD")
	if _, err := b.Pull(ctx, binding, workspace.RemoteParams{Remote: "origin", Branch: "other"}, policy); err == nil {
		t.Fatal("non-fast-forward pull succeeded")
	}
	if got := gitCommand(t, binding.Path, "rev-parse", "HEAD"); got != head {
		t.Fatal("failed pull moved HEAD")
	}
}

func TestSourceControlPreservesIgnoredFilesOnCheckoutAndPull(t *testing.T) {
	b, binding, policy := sourceFixture(t)
	ctx := context.Background()
	write(t, filepath.Join(binding.Path, ".gitignore"), "local.txt\n")
	gitCommand(t, binding.Path, "add", ".gitignore")
	gitCommand(t, binding.Path, "commit", "-m", "ignore local state")
	base := gitCommand(t, binding.Path, "rev-parse", "HEAD")
	gitCommand(t, binding.Path, "switch", "-c", "incoming")
	write(t, filepath.Join(binding.Path, "local.txt"), "incoming tracked content\n")
	gitCommand(t, binding.Path, "add", "-f", "local.txt")
	gitCommand(t, binding.Path, "commit", "-m", "track local path")
	bare := t.TempDir()
	gitCommand(t, bare, "init", "--bare", "-b", "main")
	gitCommand(t, binding.Path, "remote", "add", "origin", bare)
	gitCommand(t, binding.Path, "push", "origin", "incoming")
	gitCommand(t, binding.Path, "switch", "main")
	write(t, filepath.Join(binding.Path, "local.txt"), "valuable local state\n")
	assertRetained := func() {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(binding.Path, "local.txt"))
		if err != nil || string(data) != "valuable local state\n" || gitCommand(t, binding.Path, "rev-parse", "HEAD") != base {
			t.Fatalf("operation changed ignored content or HEAD: %q %v", data, err)
		}
	}
	if err := b.Checkout(ctx, binding, workspace.CheckoutParams{Branch: "incoming"}, policy); err == nil {
		t.Fatal("checkout overwrote ignored content")
	}
	assertRetained()
	if _, err := b.Pull(ctx, binding, workspace.RemoteParams{Remote: "origin", Branch: "incoming"}, policy); err == nil {
		t.Fatal("pull overwrote ignored content")
	}
	assertRetained()
}
