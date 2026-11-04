package session_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/mcp"
	"github.com/tomanagle/harness-kit/core/permission"
	"github.com/tomanagle/harness-kit/core/runtimes/fake"
	"github.com/tomanagle/harness-kit/core/session"
)

func newManager(t *testing.T, opts session.Options, fopts ...fake.Option) *session.Manager {
	t.Helper()
	m := session.NewManager(agent.NewRegistry(fake.New(fopts...)), opts)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	return m
}

func create(t *testing.T, m *session.Manager, cfg agent.SessionConfig) session.Info {
	t.Helper()
	info, err := m.Create(context.Background(), session.CreateParams{Runtime: "fake", Config: cfg, Metadata: map[string]string{"k": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func collect(t *testing.T, m *session.Manager, sid, text string) []event.Event {
	t.Helper()
	var evs []event.Event
	for ev, err := range m.Stream(context.Background(), sid, session.PromptParams{Input: agent.Text(text)}) {
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev)
	}
	return evs
}

func types(evs []event.Event) []event.Type {
	out := make([]event.Type, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

func TestSessionLifecycle(t *testing.T) {
	m := newManager(t, session.Options{})
	info := create(t, m, agent.SessionConfig{Cwd: t.TempDir()})
	if info.State != session.StateIdle || info.RuntimeSessionID == "" || info.Metadata["k"] != "v" || !strings.HasPrefix(info.ID, "ses_") {
		t.Fatalf("info: %+v", info)
	}

	evs := collect(t, m, info.ID, "hello world")
	got := types(evs)
	if got[0] != event.TurnStarted || got[len(got)-1] != event.TurnCompleted {
		t.Fatalf("turn framing: %v", got)
	}
	var text strings.Builder
	for _, ev := range evs {
		if ev.SessionID != info.ID {
			t.Fatalf("wrong session on %+v", ev)
		}
		if d, ok := ev.Data.(event.MessageDeltaData); ok {
			text.WriteString(d.Text)
		}
	}
	if text.String() != "Echo: hello world" {
		t.Fatalf("streamed text %q", text.String())
	}
	done := evs[len(evs)-1].Data.(event.TurnCompletedData)
	if done.Status != event.TurnStatusCompleted || done.Usage == nil {
		t.Fatalf("completed: %+v", done)
	}

	// Sequence numbers are strictly increasing and replayable.
	all, truncated, err := m.Events(info.ID, 0, 0)
	if err != nil || truncated {
		t.Fatal(err, truncated)
	}
	if all[0].Type != event.SessionStarted {
		t.Fatalf("first event %s", all[0].Type)
	}
	for i := 1; i < len(all); i++ {
		if all[i].Seq != all[i-1].Seq+1 {
			t.Fatalf("non-contiguous seq at %d", i)
		}
	}

	// A second turn works after the first.
	if evs := collect(t, m, info.ID, "again"); len(evs) == 0 {
		t.Fatal("second turn produced no events")
	}

	if err := m.Close(context.Background(), info.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(context.Background(), info.ID); err != nil {
		t.Fatalf("double close: %v", err)
	}
	got2, _ := m.Get(context.Background(), info.ID)
	if got2.State != session.StateClosed {
		t.Fatalf("state after close: %s", got2.State)
	}
	if _, err := m.Prompt(context.Background(), info.ID, session.PromptParams{Input: agent.Text("x")}); !errors.Is(err, session.ErrClosed) {
		t.Fatalf("prompt after close: %v", err)
	}
	list, _ := m.List(context.Background())
	if len(list) != 1 || list[0].ID != info.ID {
		t.Fatalf("list: %+v", list)
	}
}

func TestErrors(t *testing.T) {
	m := newManager(t, session.Options{})
	ctx := context.Background()
	if _, err := m.Create(ctx, session.CreateParams{Runtime: "nope"}); !errors.Is(err, session.ErrRuntimeNotFound) {
		t.Fatalf("unknown runtime: %v", err)
	}
	if _, err := m.Create(ctx, session.CreateParams{Runtime: "fake", Config: agent.SessionConfig{Options: map[string]any{"failStart": true}}}); err == nil {
		t.Fatal("start failure should propagate")
	}
	if _, err := m.Create(ctx, session.CreateParams{Runtime: "fake", Config: agent.SessionConfig{Options: map[string]any{"unsafe": true}}}); !errors.Is(err, session.ErrUnsafe) {
		t.Fatalf("unsafe config: %v", err)
	}
	if _, err := m.Prompt(ctx, "ses_missing", session.PromptParams{}); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}
	if list, _ := m.List(ctx); len(list) != 0 {
		t.Fatalf("failed sessions must not be listed: %+v", list)
	}
}

func TestAllowUnsafe(t *testing.T) {
	m := newManager(t, session.Options{AllowUnsafe: true})
	if _, err := m.Create(context.Background(), session.CreateParams{Runtime: "fake", Config: agent.SessionConfig{Options: map[string]any{"unsafe": true}}}); err != nil {
		t.Fatal(err)
	}
}

func TestBusyAndDuplicateTurn(t *testing.T) {
	m := newManager(t, session.Options{})
	info := create(t, m, agent.SessionConfig{})
	ctx := context.Background()
	t1, err := m.Prompt(ctx, info.ID, session.PromptParams{TurnID: "t1", Input: agent.Text("[slow] go")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Prompt(ctx, info.ID, session.PromptParams{Input: agent.Text("x")}); !errors.Is(err, session.ErrBusy) {
		t.Fatalf("busy: %v", err)
	}
	if err := m.Cancel(info.ID); err != nil {
		t.Fatal(err)
	}
	res, err := t1.Wait(ctx)
	if err != nil || res.Status != event.TurnStatusCancelled {
		t.Fatalf("cancel: %+v %v", res, err)
	}
	if _, err := m.Prompt(ctx, info.ID, session.PromptParams{TurnID: "t1"}); !errors.Is(err, session.ErrDuplicateTurn) {
		t.Fatalf("duplicate: %v", err)
	}
	// Cancel with no active turn is a no-op.
	if err := m.Cancel(info.ID); err != nil {
		t.Fatal(err)
	}
}

func TestNextPromptAcceptedAfterTurnCompletedEvent(t *testing.T) {
	// A client that prompts again as soon as it observes turn.completed
	// must never see ErrBusy.
	m := newManager(t, session.Options{})
	info := create(t, m, agent.SessionConfig{})
	sub := m.Subscribe(event.ForSession(info.ID), 0)
	defer sub.Close()
	for i := 0; i < 20; i++ {
		if _, err := m.Prompt(context.Background(), info.ID, session.PromptParams{Input: agent.Text("hi")}); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		for ev := range sub.C {
			if ev.Type == event.TurnCompleted {
				break
			}
		}
	}
}

func TestApprovalFlowHost(t *testing.T) {
	m := newManager(t, session.Options{})
	info := create(t, m, agent.SessionConfig{})
	sub := m.Subscribe(event.ForSession(info.ID), 0)
	defer sub.Close()

	turn, err := m.Prompt(context.Background(), info.ID, session.PromptParams{Input: agent.Text("[approve] please")})
	if err != nil {
		t.Fatal(err)
	}
	var req permission.Request
	for ev := range sub.C {
		if ev.Type == event.ApprovalRequested {
			req = ev.Data.(permission.Request)
			break
		}
	}
	if req.SessionID != info.ID || req.TurnID != turn.ID || req.Runtime != "fake" || req.Kind != permission.KindCommand {
		t.Fatalf("request: %+v", req)
	}
	if p := m.PendingApprovals(info.ID); len(p) != 1 {
		t.Fatalf("pending: %+v", p)
	}
	if err := m.RespondApproval(req.ID, permission.Decision{Outcome: permission.Allow}); err != nil {
		t.Fatal(err)
	}
	var seen []event.Type
	for ev := range sub.C {
		seen = append(seen, ev.Type)
		if ev.Type == event.ApprovalResolved {
			d := ev.Data.(event.ApprovalResolvedData)
			if d.Source != permission.SourceHost || d.Decision.Outcome != permission.Allow {
				t.Fatalf("resolved: %+v", d)
			}
		}
		if ev.Type == event.TurnCompleted {
			break
		}
	}
	want := []event.Type{event.ApprovalResolved, event.ToolStarted, event.ToolOutput, event.ToolCompleted}
	for i, w := range want {
		if seen[i] != w {
			t.Fatalf("after approval: %v", seen)
		}
	}
}

func TestApprovalPolicyAndTimeout(t *testing.T) {
	policy := permission.Rules{{Kind: permission.KindCommand, Command: "echo *", Action: permission.ActionDeny, Message: "no echo"}}
	m := newManager(t, session.Options{Policy: policy})
	info := create(t, m, agent.SessionConfig{})
	evs := collect(t, m, info.ID, "[approve] x")
	var resolved *event.ApprovalResolvedData
	for _, ev := range evs {
		if ev.Type == event.ApprovalRequested {
			t.Fatal("policy-decided requests must not be announced")
		}
		if d, ok := ev.Data.(event.ApprovalResolvedData); ok {
			resolved = &d
		}
		if d, ok := ev.Data.(event.ToolCompletedData); ok && d.Status != event.ToolStatusDeclined {
			t.Fatalf("tool should be declined: %+v", d)
		}
	}
	if resolved == nil || resolved.Source != permission.SourcePolicy || resolved.Request == nil || resolved.Decision.Message != "no echo" {
		t.Fatalf("resolved: %+v", resolved)
	}

	m2 := newManager(t, session.Options{ApprovalTimeout: 20 * time.Millisecond})
	info2 := create(t, m2, agent.SessionConfig{})
	for _, ev := range collect(t, m2, info2.ID, "[approve] x") {
		if d, ok := ev.Data.(event.ApprovalResolvedData); ok && d.Source != permission.SourceTimeout {
			t.Fatalf("expected timeout: %+v", d)
		}
	}
}

func TestCancelWhileAwaitingApproval(t *testing.T) {
	m := newManager(t, session.Options{})
	info := create(t, m, agent.SessionConfig{})
	sub := m.Subscribe(event.ForSession(info.ID), 0)
	defer sub.Close()
	turn, _ := m.Prompt(context.Background(), info.ID, session.PromptParams{Input: agent.Text("[approve]")})
	for ev := range sub.C {
		if ev.Type == event.ApprovalRequested {
			break
		}
	}
	m.Cancel(info.ID)
	res, _ := turn.Wait(context.Background())
	if res.Status != event.TurnStatusCancelled {
		t.Fatalf("status %s", res.Status)
	}
	if p := m.PendingApprovals(""); len(p) != 0 {
		t.Fatalf("approval left pending: %+v", p)
	}
}

func TestTurnFailureAndCrash(t *testing.T) {
	m := newManager(t, session.Options{})
	info := create(t, m, agent.SessionConfig{})
	evs := collect(t, m, info.ID, "[fail]")
	d := evs[len(evs)-1].Data.(event.TurnCompletedData)
	if d.Status != event.TurnStatusFailed || d.Error == nil || d.Error.Code != "runtime_error" {
		t.Fatalf("fail: %+v", d)
	}
	if got, _ := m.Get(context.Background(), info.ID); got.State != session.StateIdle {
		t.Fatalf("session should survive a failed turn: %s", got.State)
	}

	sub := m.Subscribe(event.ForSession(info.ID), 0)
	defer sub.Close()
	evs = collect(t, m, info.ID, "[crash]")
	d = evs[len(evs)-1].Data.(event.TurnCompletedData)
	if d.Status != event.TurnStatusFailed || d.Error.Code != "runtime_exited" {
		t.Fatalf("crash: %+v", d)
	}
	for ev := range sub.C {
		if ev.Type == event.SessionClosed {
			if ev.Data.(event.SessionClosedData).Reason != "failed" {
				t.Fatalf("closed: %+v", ev.Data)
			}
			break
		}
	}
	got, _ := m.Get(context.Background(), info.ID)
	if got.State != session.StateFailed || got.Error == nil {
		t.Fatalf("state: %+v", got)
	}
}

func TestResumeAndStreamGapRecovery(t *testing.T) {
	m := newManager(t, session.Options{StreamBuffer: 2})
	info := create(t, m, agent.SessionConfig{ResumeID: "fake-existing"})
	if info.RuntimeSessionID != "fake-existing" {
		t.Fatalf("resume id: %+v", info)
	}
	started, _, _ := m.Events(info.ID, 0, 1)
	if !started[0].Data.(event.SessionStartedData).Resumed {
		t.Fatal("session.started should report resumed")
	}

	// With a 2-event buffer and a slow consumer the subscription overflows;
	// Stream must recover the dropped events from the log transparently.
	var seqs []uint64
	for ev, err := range m.Stream(context.Background(), info.ID, session.PromptParams{Input: agent.Text(strings.Repeat("word ", 300))}) {
		if err != nil {
			t.Fatal(err)
		}
		if len(seqs) < 3 {
			time.Sleep(20 * time.Millisecond) // let the producer overrun us
		}
		seqs = append(seqs, ev.Seq)
	}
	if len(seqs) < 300 {
		t.Fatalf("expected the whole turn, got %d events", len(seqs))
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("gap in stream at %d: %d -> %d", i, seqs[i-1], seqs[i])
		}
	}
}

func TestShutdownCancelsRunningTurns(t *testing.T) {
	m := session.NewManager(agent.NewRegistry(fake.New()), session.Options{})
	info := create(t, m, agent.SessionConfig{})
	turn, _ := m.Prompt(context.Background(), info.ID, session.PromptParams{Input: agent.Text("[slow]")})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if turn.Result().Status != event.TurnStatusCancelled {
		t.Fatalf("status %s", turn.Result().Status)
	}
	if got, _ := m.Get(ctx, info.ID); got.State != session.StateClosed {
		t.Fatalf("state %s", got.State)
	}
	if _, err := m.Create(ctx, session.CreateParams{Runtime: "fake"}); !errors.Is(err, session.ErrClosed) {
		t.Fatalf("create after shutdown: %v", err)
	}
}

func TestPrepareHookAndMCPWarnings(t *testing.T) {
	m := newManager(t, session.Options{Prepare: func(_ context.Context, p *session.CreateParams) ([]string, error) {
		if p.Runtime == "boom" {
			return nil, errors.New("bad config file")
		}
		p.Config.MCP = &mcp.Config{Servers: map[string]mcp.Server{"from-hook": {Command: "x"}}}
		return []string{"hook warning"}, nil
	}})
	info := create(t, m, agent.SessionConfig{})
	got := m.Warnings(info.ID)
	if len(got) != 1 || got[0] != "hook warning" {
		t.Fatalf("warnings: %q", got)
	}
	evs, _, _ := m.Events(info.ID, 0, 0)
	var sawMCP bool
	for _, ev := range evs {
		if d, ok := ev.Data.(event.RuntimeStatusData); ok && d.Status == "mcp" && strings.Contains(d.Message, "from-hook") {
			sawMCP = true
		}
	}
	if !sawMCP {
		t.Fatal("the runtime did not receive the MCP servers set by the hook")
	}
	if _, err := m.Create(context.Background(), session.CreateParams{Runtime: "boom"}); err == nil {
		t.Fatal("prepare errors must fail creation")
	}
	// Invalid MCP config is rejected before the runtime starts.
	m2 := newManager(t, session.Options{})
	_, err := m2.Create(context.Background(), session.CreateParams{Runtime: "fake", Config: agent.SessionConfig{MCP: &mcp.Config{Servers: map[string]mcp.Server{"a__b": {Command: "x"}}}}})
	if err == nil {
		t.Fatal("invalid MCP config accepted")
	}
}
