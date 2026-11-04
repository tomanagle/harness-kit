package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/runtimes/fake"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
	"github.com/tomanagle/harness-kit/core/workspace"
	gitworkspace "github.com/tomanagle/harness-kit/core/workspace/git"
)

type fileRuntime struct {
	*fake.Runtime
	fail bool
}

func (r fileRuntime) Start(ctx context.Context, cfg agent.SessionConfig, h agent.Handler) (agent.Session, error) {
	if r.fail {
		return nil, errors.New("deliberate runtime startup failure")
	}
	s, err := r.Runtime.Start(ctx, cfg, h)
	return &fileSession{Session: s, cwd: cfg.Cwd}, err
}
func (r fileRuntime) Fork(ctx context.Context, id string, cfg agent.SessionConfig, h agent.Handler) (agent.Session, error) {
	s, err := r.Runtime.Fork(ctx, id, cfg, h)
	return &fileSession{Session: s, cwd: cfg.Cwd}, err
}

type fileSession struct {
	agent.Session
	cwd string
}

func (s *fileSession) Prompt(ctx context.Context, turn agent.Turn) (agent.TurnResult, error) {
	if err := os.WriteFile(filepath.Join(s.cwd, "changed.txt"), []byte(agent.JoinText(turn.Input)+"\n"), 0600); err != nil {
		return agent.TurnResult{}, err
	}
	return s.Session.Prompt(ctx, turn)
}

func workspaceGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
func workspaceRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git unavailable")
	}
	repo := t.TempDir()
	workspaceGit(t, repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "changed.txt"), []byte("initial\n"), 0600); err != nil {
		t.Fatal(err)
	}
	workspaceGit(t, repo, "add", ".")
	workspaceGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "initial")
	return repo
}
func workspaceManager(t *testing.T, store session.Store, backend workspace.Backend, fail bool) *session.Manager {
	t.Helper()
	m := session.NewManager(agent.NewRegistry(fileRuntime{Runtime: fake.New(), fail: fail}), session.Options{Store: store, Workspace: backend})
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	return m
}
func workspacePatch(value any) settings.Patch {
	data, _ := json.Marshal(value)
	return settings.Patch{"workspace": data}
}
func workspacePrompt(t *testing.T, m *session.Manager, id, turnID, text string, patch settings.Patch) {
	t.Helper()
	turn, err := m.Prompt(context.Background(), id, session.PromptParams{TurnID: turnID, Input: agent.Text(text), Settings: patch})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := turn.Wait(context.Background()); err != nil || result.Status != "completed" {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestWorkspaceDiffSurvivesRestartAndHasTurnBoundaries(t *testing.T) {
	ctx := context.Background()
	repo := workspaceRepo(t)
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := workspaceManager(t, store, gitworkspace.New(""), false)
	info, err := m.Create(ctx, session.CreateParams{Runtime: "fake", Config: agent.SessionConfig{Cwd: repo}, Settings: workspacePatch(map[string]any{"captureDiff": true})})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(repo, "changed.txt"), []byte("preexisting dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	workspacePrompt(t, m, info.ID, "first", "first result", nil)
	workspacePrompt(t, m, info.ID, "second", "second result", nil)
	diff, err := m.TurnDiff(ctx, info.ID, "second", workspace.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !diff.Complete || !strings.Contains(diff.WorkingTree, "-first result") || !strings.Contains(diff.WorkingTree, "+second result") || strings.Contains(diff.WorkingTree, "-initial") {
		t.Fatalf("not per-turn: %+v", diff)
	}
	if refs := workspaceGit(t, repo, "for-each-ref", "--format=%(refname)", "refs/harness-kit"); refs != "" {
		t.Fatal("temporary refs retained: " + refs)
	}
	if err = m.Close(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	cold := workspaceManager(t, store, nil, false)
	diff, err = cold.TurnDiff(ctx, info.ID, "first", workspace.DiffOptions{MaxBytes: 20})
	if err != nil || !diff.Truncated || len(diff.WorkingTree)+len(diff.Index) > 20 {
		t.Fatalf("cold diff: %+v %v", diff, err)
	}
	_, err = m.ConfigureSettings(ctx, settings.ConfigureParams{Target: settings.Target{SessionID: info.ID}, Patch: workspacePatch(map[string]any{"captureDiff": false})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Restore(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	workspacePrompt(t, m, info.ID, "disabled", "disabled", nil)
	if _, err = m.TurnDiff(ctx, info.ID, "disabled", workspace.DiffOptions{}); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatal(err)
	}
	workspacePrompt(t, m, info.ID, "one-off", "one-off", workspacePatch(map[string]any{"captureDiff": true, "maxDiffBytes": 30}))
	diff, err = m.TurnDiff(ctx, info.ID, "one-off", workspace.DiffOptions{})
	if err != nil || !diff.Truncated {
		t.Fatalf("one-off: %+v %v", diff, err)
	}
	snapshot, err := m.Settings(ctx, settings.Target{SessionID: info.ID})
	if err != nil || snapshot.Values.Workspace.CaptureDiff || snapshot.Fields["workspace.captureDiff"].When != "nextTurn" {
		t.Fatalf("settings leak: %+v %v", snapshot, err)
	}
}

func TestWorktreeForkRestoreAndCleanupInvariants(t *testing.T) {
	ctx := context.Background()
	repo := workspaceRepo(t)
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	backend := gitworkspace.New("")
	m := workspaceManager(t, store, backend, false)
	root := filepath.Join(t.TempDir(), "worktrees")
	info, err := m.Create(ctx, session.CreateParams{Runtime: "fake", Config: agent.SessionConfig{Cwd: repo}, Settings: workspacePatch(map[string]any{"mode": "worktree", "root": root, "captureDiff": true})})
	if err != nil {
		t.Fatal(err)
	}
	if info.Workspace == nil || !info.Workspace.Owned || info.Cwd == repo {
		t.Fatalf("not isolated: %+v", info)
	}
	workspacePrompt(t, m, info.ID, "parent-turn", "parent content", nil)
	if _, err = m.Fork(ctx, info.ID, session.ForkParams{}); !errors.Is(err, settings.ErrScope) {
		t.Fatal("fork silently shared worktree", err)
	}
	child, err := m.Fork(ctx, info.ID, session.ForkParams{Settings: workspacePatch(map[string]any{"root": root})})
	if err != nil {
		t.Fatal(err)
	}
	if child.Cwd == info.Cwd || child.Workspace == nil || child.Workspace.SessionID != child.ID {
		t.Fatal("fork shared binding")
	}
	if _, err = m.TurnDiff(ctx, child.ID, "parent-turn", workspace.DiffOptions{}); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatal("inherited parent diff treated as child changes", err)
	}
	if err = m.RemoveWorkspace(ctx, child.ID); !errors.Is(err, session.ErrBusy) {
		t.Fatal(err)
	}
	if err = m.Close(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	if err = m.RemoveWorkspace(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Restore(ctx, child.ID); err == nil {
		t.Fatal("removed workspace silently recreated")
	}
	if err = m.Close(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	if err = m.RemoveWorkspace(ctx, info.ID); !errors.Is(err, workspace.ErrDirty) {
		t.Fatal(err)
	}
	if err = m.Delete(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(info.Cwd); err != nil {
		t.Fatal("session deletion removed user files")
	}
}

func TestWorktreeStartupFailureRetainsRecoveryRecord(t *testing.T) {
	ctx := context.Background()
	repo := workspaceRepo(t)
	m := workspaceManager(t, session.NewMemoryStore(), gitworkspace.New(""), true)
	info, err := m.Create(ctx, session.CreateParams{Runtime: "fake", Config: agent.SessionConfig{Cwd: repo}, Settings: workspacePatch(map[string]any{"mode": "worktree", "root": t.TempDir()})})
	if err == nil || info.Workspace == nil || !strings.Contains(err.Error(), info.Workspace.Path) {
		t.Fatalf("missing recovery details: %+v %v", info, err)
	}
	stored, err := m.Get(ctx, info.ID)
	if err != nil || stored.State != session.StateFailed || stored.Workspace == nil {
		t.Fatalf("orphan: %+v %v", stored, err)
	}
	if err = m.RemoveWorkspace(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceSnapshotLimitReportsIncompleteAndDoesNotBlockTurn(t *testing.T) {
	ctx := context.Background()
	repo := workspaceRepo(t)
	m := workspaceManager(t, session.NewMemoryStore(), gitworkspace.New(""), false)
	info, err := m.Create(ctx, session.CreateParams{Runtime: "fake", Config: agent.SessionConfig{Cwd: repo}, Settings: workspacePatch(map[string]any{"captureDiff": true, "maxSnapshotBytes": 1})})
	if err != nil {
		t.Fatal(err)
	}
	workspacePrompt(t, m, info.ID, "too-large", "agent still runs", nil)
	diff, err := m.TurnDiff(ctx, info.ID, "too-large", workspace.DiffOptions{})
	if err != nil || diff.Complete || !strings.Contains(diff.Error, "limit exceeded") {
		t.Fatalf("false completeness: %+v %v", diff, err)
	}
	if _, err = m.ConfigureSettings(ctx, settings.ConfigureParams{Target: settings.Target{SessionID: info.ID}, Patch: workspacePatch(map[string]any{"mode": "worktree", "root": t.TempDir()})}); !errors.Is(err, settings.ErrNewSessionRequired) {
		t.Fatal(err)
	}
}
