package server_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/rpc/client"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
	"github.com/tomanagle/harness-kit/core/rpc/server"
	"github.com/tomanagle/harness-kit/core/runtimes/fake"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
	"github.com/tomanagle/harness-kit/core/workspace"
)

type rpcWorkspace struct{}

func (rpcWorkspace) Prepare(_ context.Context, id, cwd string, _ workspace.Settings, old *workspace.Binding) (workspace.Binding, error) {
	if old != nil {
		return *old, nil
	}
	return workspace.Binding{Kind: "test", SessionID: id, Path: cwd}, nil
}
func (rpcWorkspace) Capture(_ context.Context, _ workspace.Binding, _ string, phase string, _ workspace.Settings) (workspace.Snapshot, error) {
	return workspace.Snapshot{Tree: phase, CapturedAt: time.Now().UTC()}, nil
}
func (rpcWorkspace) Diff(_ context.Context, change workspace.TurnChange, _ workspace.DiffOptions) (workspace.Diff, error) {
	return workspace.Diff{TurnChange: change, WorkingTree: strings.Repeat("diff\n", 40)}, nil
}
func (rpcWorkspace) Remove(context.Context, workspace.Binding) error { return workspace.ErrNotOwned }

func TestWorkspaceDiffRPCAndTypedClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mgr := session.NewManager(agent.NewRegistry(fake.New()), session.Options{Workspace: rpcWorkspace{}})
	srv := server.New(mgr, server.Options{})
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, sr, sw) }()
	done := make(chan struct{}, 1)
	c, err := client.Connect(ctx, cr, cw, client.Options{OnEvent: func(ev event.Event) {
		if ev.Type == event.TurnCompleted {
			done <- struct{}{}
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()); _ = cw.Close(); <-served; _ = sw.Close() })
	if !c.Info().Capabilities.Workspace {
		t.Fatal("workspace capability missing")
	}
	info, err := c.CreateSession(ctx, protocol.SessionCreateParams{Runtime: "fake", Settings: settings.Patch{"workspace": []byte("{\"captureDiff\":true}")}})
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.Prompt(ctx, protocol.SessionPromptParams{SessionID: info.ID, TurnID: "rpc-turn", Input: agent.Text("hello")})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	diff, err := c.TurnDiff(ctx, protocol.SessionDiffParams{SessionID: info.ID, TurnID: id, DiffOptions: workspace.DiffOptions{MaxBytes: 13}})
	if err != nil || !diff.Complete || !diff.Truncated || len(diff.WorkingTree) != 13 {
		t.Fatalf("diff: %+v %v", diff, err)
	}
	_, err = c.TurnDiff(ctx, protocol.SessionDiffParams{SessionID: info.ID, TurnID: id, DiffOptions: workspace.DiffOptions{MaxBytes: -1}})
	if err == nil {
		t.Fatal("negative limit accepted")
	}
	_, err = c.TurnDiff(ctx, protocol.SessionDiffParams{SessionID: info.ID, TurnID: "missing"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatal(err)
	}
	if err = c.CloseSession(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	if err = c.RemoveWorkspace(ctx, info.ID); err == nil {
		t.Fatal("nonowned workspace removed")
	}
}
