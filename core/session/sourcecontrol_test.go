package session_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/runtimes/fake"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
	"github.com/tomanagle/harness-kit/core/workspace"
	gitworkspace "github.com/tomanagle/harness-kit/core/workspace/git"
)

func gitSession(t *testing.T, m *session.Manager, cwd string) session.Info {
	t.Helper()
	info, err := m.Create(context.Background(), session.CreateParams{Runtime: "fake", Config: agent.SessionConfig{Cwd: cwd}})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestGitLazyBindingAndLivePolicy(t *testing.T) {
	ctx := context.Background()
	repo := workspaceRepo(t)
	store := session.NewMemoryStore()
	m := session.NewManager(agent.NewRegistry(fake.New()), session.Options{Store: store, Workspace: gitworkspace.New("")})
	t.Cleanup(func() { _ = m.Shutdown(ctx) })
	info := gitSession(t, m, repo)
	if info.Workspace != nil {
		t.Fatal("test requires an ordinary session without capture")
	}
	status, err := m.GitStatus(ctx, info.ID)
	if err != nil || status.Root == "" {
		t.Fatalf("status: %+v %v", status, err)
	}
	saved, err := m.Get(ctx, info.ID)
	if err != nil || saved.Workspace == nil {
		t.Fatalf("binding not saved: %+v %v", saved, err)
	}
	if _, err := m.ConfigureSettings(ctx, settings.ConfigureParams{Patch: settings.Patch{"git": []byte(`{"readOnly":true,"maxOutputBytes":64}`)}}); err != nil {
		t.Fatal(err)
	}
	if err := m.GitStage(ctx, info.ID, workspace.PathsParams{Paths: []string{"changed.txt"}}); !errors.Is(err, workspace.ErrReadOnly) {
		t.Fatalf("write policy: %v", err)
	}
	if _, err := m.ConfigureSettings(ctx, settings.ConfigureParams{Patch: settings.Patch{"git": []byte(`{"readOnly":false,"allowRemote":false}`)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GitFetch(ctx, info.ID, workspace.RemoteParams{}); !errors.Is(err, workspace.ErrRemoteDisabled) {
		t.Fatalf("remote policy: %v", err)
	}
	if err := m.GitStage(ctx, info.ID, workspace.PathsParams{Paths: []string{"changed.txt"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ConfigureSettings(ctx, settings.ConfigureParams{Target: settings.Target{SessionID: info.ID}, Patch: settings.Patch{"git": []byte(`{"readOnly":true}`)}}); err == nil {
		t.Fatal("host policy accepted at session scope")
	}
	if err := m.Close(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	// An older record upgrades its path-only identity on first use, without a
	// runtime or a separate migration step.
	record, err := store.Get(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Info.Workspace.GitDirIdentity = ""
	record.Info.Workspace.CommonDirIdentity = ""
	if err := store.Put(ctx, record); err != nil {
		t.Fatal(err)
	}
	reopened := session.NewManager(agent.NewRegistry(), session.Options{Store: store, Workspace: gitworkspace.New("")})
	t.Cleanup(func() { _ = reopened.Shutdown(ctx) })
	if _, err := reopened.GitStatus(ctx, info.ID); err != nil {
		t.Fatalf("stored session needs no runtime: %v", err)
	}
	record, err = store.Get(ctx, info.ID)
	if err != nil || record.Info.Workspace.GitDirIdentity == "" || record.Info.Workspace.CommonDirIdentity == "" {
		t.Fatalf("legacy identity not upgraded: %+v %v", record.Info.Workspace, err)
	}
	// Replacing the repository behind a saved path must not retarget operations.
	if err := os.Rename(filepath.Join(repo, ".git"), filepath.Join(repo, "old-git")); err != nil {
		t.Fatal(err)
	}
	workspaceGit(t, repo, "init")
	if _, err := reopened.GitStatus(ctx, info.ID); err == nil {
		t.Fatal("replaced repository identity accepted")
	}
}

type waitingGit struct {
	*gitworkspace.Backend
	started, release chan struct{}
}

func (b *waitingGit) Stage(ctx context.Context, w workspace.Binding, p workspace.PathsParams, s workspace.GitSettings) error {
	close(b.started)
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return b.Backend.Stage(ctx, w, p, s)
}

func TestGitMutationReservesOnlyItsCheckout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	repo := workspaceRepo(t)
	otherRepo := workspaceRepo(t)
	backend := &waitingGit{Backend: gitworkspace.New(""), started: make(chan struct{}), release: make(chan struct{})}
	m := session.NewManager(agent.NewRegistry(fake.New()), session.Options{Workspace: backend})
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	a := gitSession(t, m, repo)
	same := gitSession(t, m, repo)
	other := gitSession(t, m, otherRepo)
	staged := make(chan error, 1)
	go func() { staged <- m.GitStage(ctx, a.ID, workspace.PathsParams{Paths: []string{"changed.txt"}}) }()
	select {
	case <-backend.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := m.Prompt(ctx, same.ID, session.PromptParams{Input: agent.Text("blocked")}); !errors.Is(err, session.ErrBusy) {
		t.Fatalf("same checkout admitted: %v", err)
	}
	turn, err := m.Prompt(ctx, other.ID, session.PromptParams{Input: agent.Text("unrelated")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.GitStage(ctx, same.ID, workspace.PathsParams{Paths: []string{"changed.txt"}}); !errors.Is(err, session.ErrBusy) {
		t.Fatalf("concurrent write admitted: %v", err)
	}
	close(backend.release)
	if err := <-staged; err != nil {
		t.Fatal(err)
	}
	turn, err = m.Prompt(ctx, same.ID, session.PromptParams{Input: agent.Text("now allowed")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestGitRefusesWritesDuringAnotherSessionsTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	repo := workspaceRepo(t)
	m := session.NewManager(agent.NewRegistry(fake.New(fake.WithDelay(time.Second))), session.Options{Workspace: gitworkspace.New("")})
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	a := gitSession(t, m, repo)
	b := gitSession(t, m, repo)
	turn, err := m.Prompt(ctx, a.ID, session.PromptParams{Input: agent.Text("several words keep this turn running")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.GitStatus(ctx, b.ID); err != nil {
		t.Fatalf("read during turn: %v", err)
	}
	if err := m.GitStage(ctx, b.ID, workspace.PathsParams{Paths: []string{"changed.txt"}}); !errors.Is(err, session.ErrBusy) {
		t.Fatalf("active sibling turn accepted write: %v", err)
	}
	if _, err := m.ConfigureSettings(ctx, settings.ConfigureParams{Patch: settings.Patch{"git": []byte(`{"allowDuringTurn":true}`)}}); err != nil {
		t.Fatal(err)
	}
	if err := m.GitStage(ctx, b.ID, workspace.PathsParams{Paths: []string{"changed.txt"}}); err != nil {
		t.Fatal(err)
	}
	_ = m.Cancel(a.ID)
	_, _ = turn.Wait(ctx)
}
