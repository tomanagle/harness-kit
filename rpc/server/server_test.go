package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/agent"
	"github.com/tomanagle/harness-kit/event"
	"github.com/tomanagle/harness-kit/mcp"
	mcpclient "github.com/tomanagle/harness-kit/mcp/client"
	"github.com/tomanagle/harness-kit/permission"
	"github.com/tomanagle/harness-kit/rpc/client"
	"github.com/tomanagle/harness-kit/rpc/protocol"
	"github.com/tomanagle/harness-kit/rpc/server"
	"github.com/tomanagle/harness-kit/runtimes/claude"
	"github.com/tomanagle/harness-kit/runtimes/fake"
	"github.com/tomanagle/harness-kit/session"
	"github.com/tomanagle/harness-kit/toolhost"
	"github.com/tomanagle/harness-kit/tools"
)

type harness struct {
	c      *client.Client
	events chan event.Event
	mgr    *session.Manager
}

func start(t *testing.T) *harness {
	t.Helper()
	mgr := session.NewManager(agent.NewRegistry(fake.New()), session.Options{})
	srv := server.New(mgr, server.Options{Version: "test"})
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(context.Background(), sr, sw) }()

	h := &harness{events: make(chan event.Event, 10000), mgr: mgr}
	c, err := client.Connect(context.Background(), cr, cw, client.Options{OnEvent: func(ev event.Event) { h.events <- ev }})
	if err != nil {
		t.Fatal(err)
	}
	h.c = c
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c.Shutdown(ctx)
		cw.Close()
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
		sw.Close()
	})
	return h
}

func (h *harness) waitFor(t *testing.T, typ event.Type) event.Event {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-h.events:
			if ev.Type == typ {
				return ev
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", typ)
		}
	}
}

func TestEndToEnd(t *testing.T) {
	h := start(t)
	ctx := context.Background()

	info := h.c.Info()
	if info.ProtocolVersion != protocol.Version || info.ServerInfo.Version != "test" || len(info.Capabilities.Runtimes) != 1 {
		t.Fatalf("initialize: %+v", info)
	}

	disc, err := h.c.Discover(ctx)
	if err != nil || len(disc.Runtimes) != 1 || !disc.Runtimes[0].Ready {
		t.Fatalf("discover: %+v %v", disc, err)
	}

	sess, err := h.c.CreateSession(ctx, protocol.SessionCreateParams{Runtime: "fake", Cwd: "/tmp", Metadata: map[string]string{"app": "test"}})
	if err != nil {
		t.Fatal(err)
	}
	if sess.State != session.StateIdle || sess.Metadata["app"] != "test" {
		t.Fatalf("session: %+v", sess)
	}
	h.waitFor(t, event.SessionStarted)

	turnID, err := h.c.Prompt(ctx, protocol.SessionPromptParams{SessionID: sess.ID, TurnID: "turn-1", Input: agent.Text("hello")})
	if err != nil || turnID != "turn-1" {
		t.Fatalf("prompt: %q %v", turnID, err)
	}
	var text strings.Builder
	for {
		ev := <-h.events
		if ev.TurnID != "turn-1" {
			continue
		}
		if d, ok := ev.Data.(event.MessageDeltaData); ok {
			text.WriteString(d.Text)
		}
		if ev.Type == event.TurnCompleted {
			if ev.Data.(event.TurnCompletedData).Status != event.TurnStatusCompleted {
				t.Fatalf("turn: %+v", ev.Data)
			}
			break
		}
	}
	if text.String() != "Echo: hello" {
		t.Fatalf("text %q", text.String())
	}

	replay, err := h.c.Events(ctx, protocol.SessionEventsParams{SessionID: sess.ID})
	if err != nil || len(replay.Events) < 4 || replay.Events[0].Seq != 1 {
		t.Fatalf("events: %+v %v", replay, err)
	}

	got, err := h.c.GetSession(ctx, sess.ID)
	if err != nil || got.LastSeq != replay.Events[len(replay.Events)-1].Seq {
		t.Fatalf("get: %+v %v", got, err)
	}
	if list, err := h.c.ListSessions(ctx); err != nil || len(list) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if err := h.c.CloseSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, event.SessionClosed)
}

func TestApprovalAndCancelOverRPC(t *testing.T) {
	h := start(t)
	ctx := context.Background()
	sess, _ := h.c.CreateSession(ctx, protocol.SessionCreateParams{Runtime: "fake"})

	if _, err := h.c.Prompt(ctx, protocol.SessionPromptParams{SessionID: sess.ID, Input: agent.Text("[approve]")}); err != nil {
		t.Fatal(err)
	}
	req := h.waitFor(t, event.ApprovalRequested).Data.(permission.Request)
	pending, err := h.c.PendingApprovals(ctx, sess.ID)
	if err != nil || len(pending) != 1 || pending[0].ID != req.ID {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	var ce *client.Error
	if err := h.c.RespondApproval(ctx, "apr_missing", permission.Decision{Outcome: permission.Allow}); !errors.As(err, &ce) || ce.Reason != protocol.ReasonApprovalNotFound {
		t.Fatalf("missing approval: %v", err)
	}
	if err := h.c.RespondApproval(ctx, req.ID, permission.Decision{Outcome: "sure"}); !errors.As(err, &ce) || ce.Reason != protocol.ReasonInvalidDecision {
		t.Fatalf("bad outcome: %v", err)
	}
	if err := h.c.RespondApproval(ctx, req.ID, permission.Decision{Outcome: permission.Deny, Message: "not today"}); err != nil {
		t.Fatal(err)
	}
	tool := h.waitFor(t, event.ToolCompleted).Data.(event.ToolCompletedData)
	if tool.Status != event.ToolStatusDeclined || tool.Error != "not today" {
		t.Fatalf("tool: %+v", tool)
	}
	h.waitFor(t, event.TurnCompleted)

	if _, err := h.c.Prompt(ctx, protocol.SessionPromptParams{SessionID: sess.ID, Input: agent.Text("[slow]")}); err != nil {
		t.Fatal(err)
	}
	_, err = h.c.Prompt(ctx, protocol.SessionPromptParams{SessionID: sess.ID, Input: agent.Text("again")})
	if !errors.As(err, &ce) || ce.Code != protocol.CodeSessionBusy || ce.Reason != protocol.ReasonSessionBusy {
		t.Fatalf("busy: %v", err)
	}
	h.waitFor(t, event.MessageDelta)
	if err := h.c.Cancel(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	done := h.waitFor(t, event.TurnCompleted).Data.(event.TurnCompletedData)
	if done.Status != event.TurnStatusCancelled {
		t.Fatalf("status %s", done.Status)
	}
}

func TestErrorMapping(t *testing.T) {
	h := start(t)
	ctx := context.Background()
	var ce *client.Error
	check := func(err error, code int, reason string) {
		t.Helper()
		if !errors.As(err, &ce) || ce.Code != code || ce.Reason != reason {
			t.Fatalf("got %v, want %d/%s", err, code, reason)
		}
	}
	_, err := h.c.CreateSession(ctx, protocol.SessionCreateParams{Runtime: "missing"})
	check(err, protocol.CodeRuntimeNotFound, protocol.ReasonRuntimeNotFound)
	_, err = h.c.CreateSession(ctx, protocol.SessionCreateParams{Runtime: "fake", Cwd: "relative/dir"})
	check(err, -32602, protocol.ReasonInvalidParams)
	_, err = h.c.CreateSession(ctx, protocol.SessionCreateParams{Runtime: "fake", Options: json.RawMessage(`{"unsafe":true}`)})
	check(err, protocol.CodePolicyViolation, protocol.ReasonPolicyViolation)
	_, err = h.c.CreateSession(ctx, protocol.SessionCreateParams{Runtime: "fake", Options: json.RawMessage(`{"bogus":1}`)})
	check(err, -32602, protocol.ReasonInvalidParams)
	_, err = h.c.GetSession(ctx, "ses_nope")
	check(err, protocol.CodeSessionNotFound, protocol.ReasonSessionNotFound)
	_, err = h.c.Prompt(ctx, protocol.SessionPromptParams{SessionID: "ses_nope", Input: agent.Text("x")})
	check(err, protocol.CodeSessionNotFound, protocol.ReasonSessionNotFound)
	_, err = h.c.Discover(ctx, "missing")
	check(err, protocol.CodeRuntimeNotFound, protocol.ReasonRuntimeNotFound)

	sess, _ := h.c.CreateSession(ctx, protocol.SessionCreateParams{Runtime: "fake"})
	_, err = h.c.Prompt(ctx, protocol.SessionPromptParams{SessionID: sess.ID})
	check(err, -32602, protocol.ReasonInvalidParams)
	_, err = h.c.Prompt(ctx, protocol.SessionPromptParams{SessionID: sess.ID, Input: []agent.InputPart{{Type: "image", Path: "x.png"}}})
	check(err, -32602, protocol.ReasonInvalidParams)
}

func TestHandshakeRules(t *testing.T) {
	mgr := session.NewManager(agent.NewRegistry(fake.New()), session.Options{})
	defer mgr.Shutdown(context.Background())
	srv := server.New(mgr, server.Options{})
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go srv.Serve(context.Background(), inR, outW)
	sc := bufio.NewScanner(outR)
	send := func(s string) map[string]any {
		t.Helper()
		io.WriteString(inW, s+"\n")
		if !sc.Scan() {
			t.Fatal("no response")
		}
		var m map[string]any
		json.Unmarshal(sc.Bytes(), &m)
		return m
	}
	code := func(m map[string]any) int {
		e, _ := m["error"].(map[string]any)
		if e == nil {
			return 0
		}
		return int(e["code"].(float64))
	}

	if m := send(`{"jsonrpc":"2.0","id":1,"method":"runtime.list"}`); code(m) != protocol.CodeServerNotInitialized {
		t.Fatalf("before init: %v", m)
	}
	if m := send(`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":99,"clientInfo":{"name":"x"}}}`); code(m) != protocol.CodeUnsupportedVersion {
		t.Fatalf("bad version: %v", m)
	}
	if m := send(`{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"protocolVersion":1,"clientInfo":{"name":"x"}}}`); code(m) != 0 {
		t.Fatalf("init: %v", m)
	}
	if m := send(`{"jsonrpc":"2.0","id":4,"method":"initialize","params":{"protocolVersion":1,"clientInfo":{"name":"x"}}}`); code(m) != -32600 {
		t.Fatalf("double init: %v", m)
	}
	if m := send(`{"jsonrpc":"2.0","id":5,"method":"no.such.method"}`); code(m) != -32601 {
		t.Fatalf("unknown method: %v", m)
	}
	inW.Close()
}

func TestPipelinedRequestsAfterInitialize(t *testing.T) {
	// Clients may send requests right after initialize without awaiting its
	// response; they must not fail with not_initialized.
	for i := 0; i < 20; i++ {
		mgr := session.NewManager(agent.NewRegistry(fake.New()), session.Options{})
		srv := server.New(mgr, server.Options{})
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		go srv.Serve(context.Background(), inR, outW)
		go io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientInfo":{"name":"x"}}}`+"\n"+
			`{"jsonrpc":"2.0","id":2,"method":"session.create","params":{"runtime":"fake"}}`+"\n")
		sc := bufio.NewScanner(outR)
		for got := 0; got < 2; {
			if !sc.Scan() {
				t.Fatal("no response")
			}
			var m map[string]any
			json.Unmarshal(sc.Bytes(), &m)
			if _, isResponse := m["id"]; !isResponse {
				continue // event notification
			}
			if m["error"] != nil {
				t.Fatalf("pipelined request failed: %v", m)
			}
			got++
		}
		inW.Close()
		mgr.Shutdown(context.Background())
	}
}

func TestSessionsOverRPC(t *testing.T) {
	h := start(t)
	ctx := context.Background()
	var ce *client.Error

	sess, _ := h.c.CreateSession(ctx, protocol.SessionCreateParams{Runtime: "fake", Cwd: "/tmp"})
	if _, err := h.c.RestoreSession(ctx, sess.ID); err != nil {
		t.Fatalf("restore live session: %v", err)
	}
	native, err := h.c.RuntimeSessions(ctx, protocol.RuntimeSessionsParams{Runtime: "fake", Cwd: "/tmp"})
	if err != nil || len(native) != 1 || native[0].ID != sess.RuntimeSessionID {
		t.Fatalf("runtime.sessions: %+v %v", native, err)
	}
	if _, err := h.c.RuntimeSessions(ctx, protocol.RuntimeSessionsParams{Runtime: "nope"}); !errors.As(err, &ce) || ce.Reason != protocol.ReasonRuntimeNotFound {
		t.Fatalf("unknown runtime: %v", err)
	}

	// Closed without any turn: nothing to resume.
	h.c.CloseSession(ctx, sess.ID)
	if _, err := h.c.RestoreSession(ctx, sess.ID); !errors.As(err, &ce) || ce.Reason != protocol.ReasonNotResumable {
		t.Fatalf("not resumable: %v", err)
	}

	// With a turn, a closed session restores under the same ID.
	s2, _ := h.c.CreateSession(ctx, protocol.SessionCreateParams{Runtime: "fake"})
	h.c.Prompt(ctx, protocol.SessionPromptParams{SessionID: s2.ID, Input: agent.Text("hi")})
	h.waitFor(t, event.TurnCompleted)
	h.c.CloseSession(ctx, s2.ID)
	restored, err := h.c.RestoreSession(ctx, s2.ID)
	if err != nil || restored.ID != s2.ID || restored.State != session.StateIdle || restored.Turns != 1 || restored.Title != "hi" {
		t.Fatalf("restore: %+v %v", restored, err)
	}
	if r, err := h.c.RenameSession(ctx, s2.ID, "Greeting"); err != nil || r.Title != "Greeting" {
		t.Fatalf("rename: %+v %v", r, err)
	}
	if _, err := h.c.RenameSession(ctx, s2.ID, " "); !errors.As(err, &ce) || ce.Reason != protocol.ReasonInvalidParams {
		t.Fatalf("rename to empty: %v", err)
	}
	if _, err := h.c.RenameSession(ctx, "ses_missing", "x"); !errors.As(err, &ce) || ce.Reason != protocol.ReasonSessionNotFound {
		t.Fatalf("rename missing: %v", err)
	}
	if named, err := h.c.CreateSession(ctx, protocol.SessionCreateParams{Runtime: "fake", Title: "Named"}); err != nil || named.Title != "Named" {
		t.Fatalf("create with title: %+v %v", named, err)
	}
	if err := h.c.DeleteSession(ctx, s2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.GetSession(ctx, s2.ID); !errors.As(err, &ce) || ce.Reason != protocol.ReasonSessionNotFound {
		t.Fatalf("after delete: %v", err)
	}
}

func TestUnsupportedSessionListing(t *testing.T) {
	mgr := session.NewManager(agent.NewRegistry(claude.New(claude.WithPath("/nope"))), session.Options{})
	defer mgr.Shutdown(context.Background())
	srv := server.New(mgr, server.Options{})
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	go srv.Serve(context.Background(), sr, sw)
	c, err := client.Connect(context.Background(), cr, cw, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cw.Close()
	var ce *client.Error
	if _, err := c.RuntimeSessions(context.Background(), protocol.RuntimeSessionsParams{Runtime: "claude"}); !errors.As(err, &ce) || ce.Code != protocol.CodeUnsupported || ce.Reason != protocol.ReasonUnsupported {
		t.Fatalf("got %v", err)
	}
}

// TestMain lets the test binary act as the tool bridge.
func TestMain(m *testing.M) {
	toolhost.ServeBridgeIfRequested()
	os.Exit(m.Run())
}

func callViaMCP(ctx context.Context, s mcp.Server, tool string, args json.RawMessage) (string, bool, error) {
	s.Env["GORACE"] = "atexit_sleep_ms=0"
	sess, err := mcpclient.Connect(ctx, s, mcpclient.Options{})
	if err != nil {
		return "", false, err
	}
	defer sess.Close()
	res, err := sess.CallTool(ctx, tool, args)
	return res.Text(), res.IsError, err
}

func TestHostToolsOverRPC(t *testing.T) {
	ctx := context.Background()
	reg := tools.NewRegistry()
	exe, _ := os.Executable()
	host := toolhost.New(toolhost.Options{Registry: reg, BridgeCommand: []string{exe}})
	defer host.Close()
	mgr := session.NewManager(agent.NewRegistry(fake.New(fake.WithMCPCaller(callViaMCP))), session.Options{ToolServer: host})
	defer mgr.Shutdown(ctx)
	srv := server.New(mgr, server.Options{Tools: reg})

	connect := func() (*client.Client, chan event.Event, func()) {
		cr, sw := io.Pipe()
		sr, cw := io.Pipe()
		go srv.Serve(ctx, sr, sw)
		events := make(chan event.Event, 1000)
		c, err := client.Connect(ctx, cr, cw, client.Options{OnEvent: func(ev event.Event) { events <- ev }})
		if err != nil {
			t.Fatal(err)
		}
		return c, events, func() { cw.Close(); sw.Close() }
	}
	c, events, disconnect := connect()
	if !c.Info().Capabilities.Tools {
		t.Fatal("server should advertise tools")
	}

	var gotCall protocol.ToolCallParams
	err := c.RegisterTool(ctx, protocol.ToolSpec{
		Name: "lookup", Description: "Look up an order",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
	}, func(_ context.Context, call protocol.ToolCallParams) (tools.Result, error) {
		gotCall = call
		var in struct{ ID string }
		json.Unmarshal(call.Arguments, &in)
		return tools.TextResult("order " + in.ID + " shipped"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c.RegisterTool(ctx, protocol.ToolSpec{Name: "refund", Approval: mcp.ApprovalAsk}, func(context.Context, protocol.ToolCallParams) (tools.Result, error) {
		return tools.TextResult("refunded"), nil
	})
	var ce *client.Error
	if err := c.RegisterTool(ctx, protocol.ToolSpec{Name: "lookup"}, nil); !errors.As(err, &ce) || ce.Reason != protocol.ReasonDuplicateTool {
		t.Fatalf("duplicate: %v", err)
	}
	if list, _ := c.Tools(ctx); len(list) != 2 {
		t.Fatalf("tools.list: %+v", list)
	}

	sess, err := c.CreateSession(ctx, protocol.SessionCreateParams{Runtime: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	waitTurn := func() (out []event.Event) {
		t.Helper()
		timeout := time.After(20 * time.Second)
		for {
			select {
			case ev := <-events:
				if ev.SessionID != sess.ID {
					continue
				}
				out = append(out, ev)
				if req, ok := ev.Data.(permission.Request); ok {
					c.RespondApproval(ctx, req.ID, permission.Decision{Outcome: permission.Allow})
				}
				if ev.Type == event.TurnCompleted {
					return out
				}
			case <-timeout:
				t.Fatal("turn did not finish")
			}
		}
	}

	turnID, _ := c.Prompt(ctx, protocol.SessionPromptParams{SessionID: sess.ID, Input: agent.Text(`[call lookup {"id":"A7"}]`)})
	evs := waitTurn()
	var completed *event.ToolCompletedData
	for _, ev := range evs {
		if d, ok := ev.Data.(event.ToolCompletedData); ok {
			completed = &d
		}
	}
	if completed == nil || completed.Status != event.ToolStatusCompleted || completed.Output != "order A7 shipped" {
		t.Fatalf("tool result: %+v", completed)
	}
	if gotCall.SessionID != sess.ID || gotCall.TurnID != turnID || gotCall.Name != "lookup" {
		t.Fatalf("tool.call params: %+v (turn %s)", gotCall, turnID)
	}

	// Invalid arguments never reach the client; approval: ask asks first.
	gotCall = protocol.ToolCallParams{}
	c.Prompt(ctx, protocol.SessionPromptParams{SessionID: sess.ID, Input: agent.Text(`[call lookup {"id":5}] [call refund]`)})
	evs = waitTurn()
	var outputs []string
	sawApproval := false
	for _, ev := range evs {
		if d, ok := ev.Data.(event.ToolCompletedData); ok {
			outputs = append(outputs, string(d.Status)+":"+d.Output+d.Error)
		}
		if ev.Type == event.ApprovalRequested {
			sawApproval = ev.Data.(permission.Request).Tool == "refund"
		}
	}
	if gotCall.Name != "" || len(outputs) != 2 || !strings.Contains(outputs[0], "expected string") || outputs[1] != "completed:refunded" || !sawApproval {
		t.Fatalf("second turn: %q approval=%v call=%+v", outputs, sawApproval, gotCall)
	}

	// A client's tools disappear when it disconnects.
	disconnect()
	deadline := time.Now().Add(5 * time.Second)
	for len(reg.List()) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(reg.List()) != 0 {
		t.Fatalf("tools left after disconnect: %+v", reg.List())
	}
}
