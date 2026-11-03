package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/agent"
	"github.com/tomanagle/harness-kit/event"
	"github.com/tomanagle/harness-kit/mcp"
	"github.com/tomanagle/harness-kit/permission"
)

// The test binary doubles as a scripted fake `codex` executable.
func TestMain(m *testing.M) {
	if os.Getenv("HK_FAKE_CODEX") == "1" {
		fakeCodex()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeCodex() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--version" {
		fmt.Println("codex-cli 9.9.9")
		return
	}
	var wmu sync.Mutex
	out := bufio.NewWriter(os.Stdout)
	send := func(v map[string]any) {
		b, _ := json.Marshal(v)
		wmu.Lock()
		out.Write(append(b, '\n'))
		out.Flush()
		wmu.Unlock()
	}
	notify := func(method string, params map[string]any) { send(map[string]any{"method": method, "params": params}) }

	var mu sync.Mutex
	nextID := 1000
	pending := map[float64]chan json.RawMessage{}
	request := func(method string, params map[string]any) json.RawMessage {
		mu.Lock()
		nextID++
		id := float64(nextID)
		ch := make(chan json.RawMessage, 1)
		pending[id] = ch
		mu.Unlock()
		send(map[string]any{"id": id, "method": method, "params": params})
		return <-ch
	}
	interrupted := make(chan struct{}, 1)

	runTurn := func(thread, turn, text string) {
		notify("turn/started", map[string]any{"threadId": thread, "turn": map[string]any{"id": turn, "status": "inProgress", "items": []any{}}})
		complete := func(status string, errMsg string) {
			t := map[string]any{"id": turn, "status": status, "items": []any{}}
			if errMsg != "" {
				t["error"] = map[string]any{"message": errMsg}
			}
			notify("turn/completed", map[string]any{"threadId": thread, "turn": t})
		}
		if strings.Contains(text, "[exit]") {
			os.Exit(3)
		}
		if strings.Contains(text, "[fail]") {
			complete("failed", "boom")
			return
		}
		if strings.Contains(text, "[approve]") {
			notify("item/started", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"type": "commandExecution", "id": "cmd_1", "command": "ls -la", "cwd": "/tmp", "status": "inProgress", "commandActions": []any{}}})
			resp := request("item/commandExecution/requestApproval", map[string]any{"threadId": thread, "turnId": turn, "itemId": "cmd_1", "command": "ls -la", "cwd": "/tmp", "reason": "list files", "startedAtMs": 1})
			var r struct{ Decision string }
			json.Unmarshal(resp, &r)
			switch r.Decision {
			case "accept", "acceptForSession":
				notify("item/commandExecution/outputDelta", map[string]any{"threadId": thread, "turnId": turn, "itemId": "cmd_1", "delta": "file\n"})
				notify("item/completed", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"type": "commandExecution", "id": "cmd_1", "command": "ls -la", "cwd": "/tmp", "status": "completed", "aggregatedOutput": "file\n", "exitCode": 0, "commandActions": []any{}}})
			case "cancel":
				complete("interrupted", "")
				return
			default:
				notify("item/completed", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"type": "commandExecution", "id": "cmd_1", "command": "ls -la", "cwd": "/tmp", "status": "declined", "commandActions": []any{}}})
			}
			// File change with its own approval.
			notify("item/started", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"type": "fileChange", "id": "fc_1", "status": "inProgress", "changes": []any{map[string]any{"path": "main.go", "kind": map[string]any{"type": "update"}, "diff": "+x"}}}})
			resp = request("item/fileChange/requestApproval", map[string]any{"threadId": thread, "turnId": turn, "itemId": "fc_1", "startedAtMs": 1})
			json.Unmarshal(resp, &r)
			st := "completed"
			if r.Decision != "accept" && r.Decision != "acceptForSession" {
				st = "declined"
			}
			notify("item/completed", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"type": "fileChange", "id": "fc_1", "status": st, "changes": []any{}}})
		}
		if strings.Contains(text, "[mcp]") {
			notify("item/started", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"type": "mcpToolCall", "id": "mcp_1", "server": "kit", "tool": "echo", "arguments": map[string]any{"text": "ping"}, "status": "inProgress"}})
			resp := request("mcpServer/elicitation/request", map[string]any{"threadId": thread, "turnId": turn, "serverName": "kit", "mode": "form",
				"message": "Allow kit to run echo?", "requestedSchema": map[string]any{"type": "object", "properties": map[string]any{}},
				"_meta": map[string]any{"codex_approval_kind": "mcp_tool_call", "tool_description": "Echo text", "tool_params": map[string]any{"text": "ping"}}})
			var r struct{ Action string }
			json.Unmarshal(resp, &r)
			st := "failed"
			if r.Action == "accept" {
				st = "completed"
			}
			notify("item/completed", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"type": "mcpToolCall", "id": "mcp_1", "server": "kit", "tool": "echo", "arguments": map[string]any{}, "status": st}})
			if r.Action == "cancel" {
				complete("interrupted", "")
				return
			}
			// A non-approval elicitation (a form) must be declined.
			resp = request("mcpServer/elicitation/request", map[string]any{"threadId": thread, "serverName": "kit", "mode": "form", "message": "Your email?", "requestedSchema": map[string]any{"type": "object"}})
			json.Unmarshal(resp, &r)
			if r.Action != "decline" {
				complete("failed", "form elicitation was not declined: "+r.Action)
				return
			}
			complete("completed", "")
			return
		}
		if strings.Contains(text, "[slow]") {
			notify("item/agentMessage/delta", map[string]any{"threadId": thread, "turnId": turn, "itemId": "msg_1", "delta": "thinking..."})
			select {
			case <-interrupted:
				complete("interrupted", "")
			case <-time.After(10 * time.Second):
				complete("completed", "")
			}
			return
		}
		notify("item/started", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"type": "agentMessage", "id": "msg_1", "text": ""}})
		notify("item/reasoning/summaryTextDelta", map[string]any{"threadId": thread, "turnId": turn, "itemId": "rs_1", "delta": "hmm", "summaryIndex": 0})
		notify("item/agentMessage/delta", map[string]any{"threadId": thread, "turnId": turn, "itemId": "msg_1", "delta": "Hello "})
		notify("item/agentMessage/delta", map[string]any{"threadId": thread, "turnId": turn, "itemId": "msg_1", "delta": "from codex"})
		notify("item/completed", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"type": "agentMessage", "id": "msg_1", "text": "Hello from codex"}})
		notify("thread/tokenUsage/updated", map[string]any{"threadId": thread, "turnId": turn, "tokenUsage": map[string]any{
			"total":              map[string]any{"inputTokens": 120, "cachedInputTokens": 20, "outputTokens": 30, "reasoningOutputTokens": 5, "totalTokens": 150},
			"last":               map[string]any{"inputTokens": 120, "cachedInputTokens": 20, "outputTokens": 30, "reasoningOutputTokens": 5, "totalTokens": 150},
			"modelContextWindow": 200000,
		}})
		notify("thread/archived", map[string]any{"threadId": thread}) // unmapped -> raw
		notify("item/agentMessage/delta", map[string]any{"threadId": "other-thread", "turnId": "x", "itemId": "sub", "delta": "subagent"})
		complete("completed", "")
	}

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	initialized := false
	turnN := 0
	for in.Scan() {
		var msg struct {
			ID     *float64        `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(in.Bytes(), &msg) != nil {
			continue
		}
		if log := os.Getenv("HK_FAKE_CODEX_REQUEST_LOG"); log != "" {
			f, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err == nil {
				fmt.Fprintln(f, in.Text())
				f.Close()
			}
		}
		if strings.Contains(in.Text(), `"jsonrpc"`) {
			fmt.Fprintln(os.Stderr, "fake codex: client sent jsonrpc member despite lite framing")
			os.Exit(4)
		}
		if msg.Method == "" && msg.ID != nil {
			mu.Lock()
			ch := pending[*msg.ID]
			delete(pending, *msg.ID)
			mu.Unlock()
			if ch != nil {
				ch <- msg.Result
			}
			continue
		}
		reply := func(result any) { send(map[string]any{"id": *msg.ID, "result": result}) }
		switch msg.Method {
		case "initialize":
			var p struct {
				ClientInfo struct{ Name string } `json:"clientInfo"`
			}
			json.Unmarshal(msg.Params, &p)
			if p.ClientInfo.Name == "" {
				send(map[string]any{"id": *msg.ID, "error": map[string]any{"code": -32600, "message": "clientInfo required"}})
				continue
			}
			reply(map[string]any{"userAgent": "fake", "codexHome": "/tmp", "platformFamily": "unix", "platformOs": "macos"})
		case "initialized":
			initialized = true
		case "account/read":
			if os.Getenv("HK_FAKE_CODEX_AUTH") == "none" {
				reply(map[string]any{"account": nil, "requiresOpenaiAuth": true})
			} else {
				reply(map[string]any{"account": map[string]any{"type": "chatgpt", "planType": "pro", "email": "user@example.com"}, "requiresOpenaiAuth": true})
			}
		case "thread/start", "thread/resume", "thread/fork":
			if !initialized {
				send(map[string]any{"id": *msg.ID, "error": map[string]any{"code": -32600, "message": "Not initialized"}})
				continue
			}
			var p struct {
				ThreadID string          `json:"threadId"`
				Model    string          `json:"model"`
				Sandbox  string          `json:"sandbox"`
				Config   json.RawMessage `json:"config"`
			}
			json.Unmarshal(msg.Params, &p)
			if f := os.Getenv("HK_FAKE_CODEX_CONFIG_LOG"); f != "" {
				os.WriteFile(f, p.Config, 0o600)
			}
			id := "thr_new"
			if p.ThreadID != "" {
				id = p.ThreadID
			}
			if msg.Method == "thread/fork" {
				if p.ThreadID == "" {
					panic("missing fork threadId")
				}
				id = "forked-" + p.ThreadID
			}
			model := p.Model
			if model == "" {
				model = "gpt-test"
			}
			response := map[string]any{"thread": map[string]any{"id": id}, "model": model, "sandbox": map[string]any{"type": p.Sandbox}}
			if effort := os.Getenv("HK_FAKE_CODEX_RESOLVED_EFFORT"); effort != "" {
				response["reasoningEffort"] = effort
			}
			reply(response)
		case "turn/start":
			var p struct {
				ThreadID string `json:"threadId"`
				Input    []struct{ Type, Text string }
			}
			json.Unmarshal(msg.Params, &p)
			turnN++
			turnID := fmt.Sprintf("turn_%d", turnN)
			reply(map[string]any{"turn": map[string]any{"id": turnID, "status": "inProgress", "items": []any{}}})
			go runTurn(p.ThreadID, turnID, p.Input[0].Text)
		case "config/read":
			cfg := map[string]any{"mcp_servers": map[string]any{"native_a": map[string]any{"command": "a"}, "clash": map[string]any{"command": "c"}}}
			if os.Getenv("HK_FAKE_CODEX_NO_DEFAULTS") == "" {
				cfg["model"] = "gpt-test"
			}
			reply(map[string]any{"config": cfg})
		case "model/list":
			data := []map[string]any{}
			if os.Getenv("HK_FAKE_CODEX_NO_DEFAULTS") == "" {
				data = append(data,
					map[string]any{"id": "gpt-test", "model": "gpt-test", "defaultReasoningEffort": "medium", "isDefault": true},
					map[string]any{"id": "gpt-other", "model": "gpt-other", "defaultReasoningEffort": "low", "isDefault": false})
			}
			reply(map[string]any{"data": data, "nextCursor": nil})
		case "thread/list":
			var p struct {
				Cwd         string   `json:"cwd"`
				Limit       int      `json:"limit"`
				SortKey     string   `json:"sortKey"`
				SourceKinds []string `json:"sourceKinds"`
			}
			json.Unmarshal(msg.Params, &p)
			if p.SortKey != "updated_at" || len(p.SourceKinds) == 0 {
				send(map[string]any{"id": *msg.ID, "error": map[string]any{"code": -32602, "message": "unexpected params"}})
				continue
			}
			threads := []any{
				map[string]any{"id": "thr_named", "cwd": "/work", "name": "Refactor auth", "preview": "please refactor", "model": "gpt-test", "source": "cli", "createdAt": 1700000000, "updatedAt": 1700000500},
				map[string]any{"id": "thr_preview", "cwd": "/other", "name": nil, "preview": "  explain\nthis   code ", "source": map[string]any{"custom": "x"}, "createdAt": 1700000100, "updatedAt": 1700000200},
			}
			var data []any
			for _, th := range threads {
				if p.Cwd == "" || th.(map[string]any)["cwd"] == p.Cwd {
					data = append(data, th)
				}
			}
			if p.Limit > 0 && len(data) > p.Limit {
				data = data[:p.Limit]
			}
			reply(map[string]any{"data": data, "nextCursor": nil})
		case "turn/interrupt":
			reply(map[string]any{})
			select {
			case interrupted <- struct{}{}:
			default:
			}
		default:
			send(map[string]any{"id": *msg.ID, "error": map[string]any{"code": -32601, "message": "unknown " + msg.Method}})
		}
	}
}

type recorder struct {
	mu        sync.Mutex
	events    []event.Event
	approvals []permission.Request
	decide    func(permission.Request) permission.Decision
}

func (r *recorder) Emit(ev event.Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recorder) RequestApproval(ctx context.Context, req permission.Request) (permission.Decision, error) {
	r.mu.Lock()
	r.approvals = append(r.approvals, req)
	decide := r.decide
	r.mu.Unlock()
	if decide == nil {
		<-ctx.Done()
		return permission.Decision{}, ctx.Err()
	}
	return decide(req), nil
}

func (r *recorder) ofType(t event.Type) []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []event.Event
	for _, e := range r.events {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

func newFake(t *testing.T, env ...string) *Runtime {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return New(WithPath(exe), WithEnv(append([]string{"HK_FAKE_CODEX=1", "GORACE=atexit_sleep_ms=0"}, env...)...), WithStartTimeout(10*time.Second))
}

func startSession(t *testing.T, rt *Runtime, cfg agent.SessionConfig, rec *recorder) *Session {
	t.Helper()
	s, err := rt.Start(context.Background(), cfg, rec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	return s.(*Session)
}

func TestDiscover(t *testing.T) {
	d, err := newFake(t).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !d.Installed || d.Version != "9.9.9" || d.Auth.State != agent.AuthAuthenticated || d.Auth.Method != "chatgpt" || d.Auth.Plan != "pro" || !d.Ready {
		t.Fatalf("discovery: %+v", d)
	}
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), "user@example.com") {
		t.Fatal("discovery must not leak the account email")
	}

	d, _ = newFake(t, "HK_FAKE_CODEX_AUTH=none").Discover(context.Background())
	if d.Auth.State != agent.AuthUnauthenticated || d.Ready {
		t.Fatalf("signed out: %+v", d)
	}

	d, _ = New(WithPath("/definitely/not/codex")).Discover(context.Background())
	if d.Installed || d.Ready || len(d.Problems) == 0 {
		t.Fatalf("missing: %+v", d)
	}
}

func TestTurnStreamsAndMapsEvents(t *testing.T) {
	rec := &recorder{}
	s := startSession(t, newFake(t), agent.SessionConfig{Cwd: t.TempDir(), RawEvents: true}, rec)
	if s.RuntimeSessionID() != "thr_new" {
		t.Fatalf("thread id %q", s.RuntimeSessionID())
	}
	res, err := s.Prompt(context.Background(), agent.Turn{ID: "t1", Input: agent.Text("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != event.TurnStatusCompleted || res.RuntimeTurnID != "turn_1" {
		t.Fatalf("result: %+v", res)
	}
	if res.Usage == nil || res.Usage.InputTokens != 120 || res.Usage.Scope != "turn" {
		t.Fatalf("usage: %+v", res.Usage)
	}
	var text string
	for _, e := range rec.ofType(event.MessageDelta) {
		d := e.Data.(event.MessageDeltaData)
		if d.ItemID == "sub" {
			t.Fatal("events from other threads must not be mapped")
		}
		if e.TurnID != "t1" {
			t.Fatalf("turn id %q", e.TurnID)
		}
		text += d.Text
	}
	if text != "Hello from codex" {
		t.Fatalf("text %q", text)
	}
	if mc := rec.ofType(event.MessageCompleted); len(mc) != 1 || mc[0].Data.(event.MessageCompletedData).Text != "Hello from codex" {
		t.Fatalf("message completed: %+v", mc)
	}
	if len(rec.ofType(event.ReasoningDelta)) != 1 {
		t.Fatal("reasoning delta missing")
	}
	if u := rec.ofType(event.Usage); len(u) != 1 || u[0].Data.(event.UsageData).ContextWindow != 200000 {
		t.Fatalf("usage events: %+v", u)
	}
	raws := rec.ofType(event.RuntimeRaw)
	if len(raws) < 2 {
		t.Fatalf("raw events: %+v", raws)
	}

	// Second turn on the same thread.
	res, err = s.Prompt(context.Background(), agent.Turn{ID: "t2", Input: agent.Text("again")})
	if err != nil || res.RuntimeTurnID != "turn_2" {
		t.Fatalf("second turn: %+v %v", res, err)
	}
}

func TestApprovals(t *testing.T) {
	rec := &recorder{decide: func(r permission.Request) permission.Decision {
		if r.Kind == permission.KindCommand {
			return permission.Decision{Outcome: permission.Allow}
		}
		return permission.Decision{Outcome: permission.Deny}
	}}
	s := startSession(t, newFake(t), agent.SessionConfig{}, rec)
	res, err := s.Prompt(context.Background(), agent.Turn{ID: "t1", Input: agent.Text("[approve]")})
	if err != nil || res.Status != event.TurnStatusCompleted {
		t.Fatalf("%+v %v", res, err)
	}
	if len(rec.approvals) != 2 {
		t.Fatalf("approvals: %+v", rec.approvals)
	}
	cmd, fc := rec.approvals[0], rec.approvals[1]
	if cmd.Command != "ls -la" || cmd.Cwd != "/tmp" || cmd.Reason != "list files" || cmd.TurnID != "t1" || !cmd.Supports(permission.AllowSession) {
		t.Fatalf("command approval: %+v", cmd)
	}
	if fc.Kind != permission.KindFileChange || len(fc.Paths) != 1 || fc.Paths[0] != "main.go" {
		t.Fatalf("file approval: %+v", fc)
	}
	tools := rec.ofType(event.ToolCompleted)
	if len(tools) != 2 {
		t.Fatalf("tools: %+v", tools)
	}
	if d := tools[0].Data.(event.ToolCompletedData); d.Status != event.ToolStatusCompleted || d.Output != "file\n" || d.ExitCode == nil || *d.ExitCode != 0 {
		t.Fatalf("command tool: %+v", d)
	}
	if d := tools[1].Data.(event.ToolCompletedData); d.Status != event.ToolStatusDeclined {
		t.Fatalf("file tool: %+v", d)
	}
	if len(rec.ofType(event.ToolOutput)) != 1 {
		t.Fatal("tool output delta missing")
	}

	// Abort maps to Codex "cancel", which interrupts the turn.
	rec.decide = func(permission.Request) permission.Decision { return permission.Decision{Outcome: permission.Abort} }
	res, err = s.Prompt(context.Background(), agent.Turn{ID: "t2", Input: agent.Text("[approve]")})
	if err != nil || res.Status != event.TurnStatusCancelled {
		t.Fatalf("abort: %+v %v", res, err)
	}
}

func TestCancelInterruptsTurn(t *testing.T) {
	rec := &recorder{}
	s := startSession(t, newFake(t), agent.SessionConfig{}, rec)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for len(rec.ofType(event.MessageDelta)) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	res, err := s.Prompt(ctx, agent.Turn{ID: "t1", Input: agent.Text("[slow]")})
	if err != nil || res.Status != event.TurnStatusCancelled {
		t.Fatalf("%+v %v", res, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("interrupt was not graceful/prompt")
	}
	// The session remains usable.
	if res, err := s.Prompt(context.Background(), agent.Turn{ID: "t2", Input: agent.Text("hi")}); err != nil || res.Status != event.TurnStatusCompleted {
		t.Fatalf("after cancel: %+v %v", res, err)
	}
}

func TestCancelWhileAwaitingApproval(t *testing.T) {
	rec := &recorder{} // never decides: blocks until ctx is cancelled
	s := startSession(t, newFake(t), agent.SessionConfig{}, rec)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			rec.mu.Lock()
			n := len(rec.approvals)
			rec.mu.Unlock()
			if n > 0 {
				cancel()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	res, err := s.Prompt(ctx, agent.Turn{ID: "t1", Input: agent.Text("[approve]")})
	if err != nil || res.Status != event.TurnStatusCancelled {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestFailureAndCrash(t *testing.T) {
	rec := &recorder{}
	s := startSession(t, newFake(t), agent.SessionConfig{}, rec)
	res, err := s.Prompt(context.Background(), agent.Turn{ID: "t1", Input: agent.Text("[fail]")})
	if err != nil || res.Status != event.TurnStatusFailed || res.Error == nil || res.Error.Message != "boom" {
		t.Fatalf("fail: %+v %v", res, err)
	}
	_, err = s.Prompt(context.Background(), agent.Turn{ID: "t2", Input: agent.Text("[exit]")})
	if err == nil || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("crash: %v", err)
	}
	if _, err := s.Prompt(context.Background(), agent.Turn{ID: "t3", Input: agent.Text("hi")}); err == nil {
		t.Fatal("dead session should refuse prompts")
	}
}

func TestResumeAndOptions(t *testing.T) {
	rt := newFake(t)
	s := startSession(t, rt, agent.SessionConfig{ResumeID: "thr_old"}, &recorder{})
	if s.RuntimeSessionID() != "thr_old" {
		t.Fatalf("resume: %q", s.RuntimeSessionID())
	}
	if _, err := rt.Start(context.Background(), agent.SessionConfig{Options: map[string]any{"sandbox": "nope"}}, &recorder{}); err == nil {
		t.Fatal("invalid sandbox should fail")
	}
	if _, err := rt.Start(context.Background(), agent.SessionConfig{Cwd: "/does/not/exist"}, &recorder{}); err == nil {
		t.Fatal("missing cwd should fail")
	}
	if rt.Unsafe(agent.SessionConfig{Options: map[string]any{"sandbox": "danger-full-access"}}) == "" {
		t.Fatal("danger-full-access must be flagged")
	}
	if rt.Unsafe(agent.SessionConfig{Options: map[string]any{"config": map[string]any{"sandbox_mode": "x"}}}) == "" {
		t.Fatal("sandbox config override must be flagged")
	}
	if rt.Unsafe(agent.SessionConfig{Options: map[string]any{"sandbox": "read-only"}}) != "" {
		t.Fatal("read-only is safe")
	}
}

func TestListSessions(t *testing.T) {
	rt := newFake(t)
	got, err := rt.ListSessions(context.Background(), agent.ListSessionsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("sessions: %+v", got)
	}
	a, b := got[0], got[1]
	if a.ID != "thr_named" || a.Title != "Refactor auth" || a.Cwd != "/work" || a.Model != "gpt-test" || a.Source != "cli" || a.UpdatedAt.Unix() != 1700000500 {
		t.Fatalf("named thread: %+v", a)
	}
	if b.Title != "explain this code" || b.Source != "other" {
		t.Fatalf("preview thread: %+v", b)
	}
	filtered, err := rt.ListSessions(context.Background(), agent.ListSessionsOptions{Cwd: "/other", Limit: 5})
	if err != nil || len(filtered) != 1 || filtered[0].ID != "thr_preview" {
		t.Fatalf("cwd filter: %+v %v", filtered, err)
	}
	if _, err := New(WithPath("/definitely/not/codex")).ListSessions(context.Background(), agent.ListSessionsOptions{}); !errors.Is(err, agent.ErrNotInstalled) {
		t.Fatalf("missing binary: %v", err)
	}
}

func TestMCPServersInThreadConfig(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "config.json")
	rt := newFake(t, "HK_FAKE_CODEX_CONFIG_LOG="+logFile, "HK_TEST_TOKEN=s3cret")
	no := false
	rec := &recorder{}
	cfg := agent.SessionConfig{
		Options: map[string]any{"config": map[string]any{"model_reasoning_summary": "auto"}},
		MCP: &mcp.Config{InheritNative: &no, Servers: map[string]mcp.Server{
			"kit":     {Command: "kit-server", Args: []string{"--token=${HK_TEST_TOKEN}"}, Env: map[string]string{"T": "${HK_TEST_TOKEN}"}, Tools: mcp.ToolFilter{Allow: []string{"echo"}, Deny: []string{"rm"}}, ToolTimeoutSec: 5},
			"web":     {Type: "http", URL: "https://mcp.example.com/mcp", Headers: map[string]string{"Authorization": "Bearer ${HK_TEST_TOKEN}"}, Approval: mcp.ApprovalAsk},
			"clash":   {Command: "mine"},
			"missing": {Command: "x", Env: map[string]string{"K": "${HK_DEFINITELY_UNSET}"}},
		}},
	}
	startSession(t, rt, cfg, rec)

	var sent struct {
		Summary    string                    `json:"model_reasoning_summary"`
		MCPServers map[string]map[string]any `json:"mcp_servers"`
	}
	b, _ := os.ReadFile(logFile)
	if err := json.Unmarshal(b, &sent); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	if sent.Summary != "auto" {
		t.Fatal("options.config passthrough lost")
	}
	kit := sent.MCPServers["kit"]
	if kit["command"] != "kit-server" || kit["args"].([]any)[0] != "--token=s3cret" || kit["env"].(map[string]any)["T"] != "s3cret" ||
		kit["enabled_tools"].([]any)[0] != "echo" || kit["disabled_tools"].([]any)[0] != "rm" || kit["default_tools_approval_mode"] != "approve" || kit["tool_timeout_sec"] != 5.0 {
		t.Fatalf("kit server: %+v", kit)
	}
	web := sent.MCPServers["web"]
	if web["url"] != "https://mcp.example.com/mcp" || web["http_headers"].(map[string]any)["Authorization"] != "Bearer s3cret" || web["default_tools_approval_mode"] != "prompt" {
		t.Fatalf("http server: %+v", web)
	}
	if sent.MCPServers["native_a"]["enabled"] != false {
		t.Fatalf("inheritNative=false must disable native servers: %+v", sent.MCPServers["native_a"])
	}
	if _, ok := sent.MCPServers["missing"]; ok {
		t.Fatal("server with an unset variable must be skipped")
	}
	if sent.MCPServers["clash"]["command"] != nil {
		t.Fatalf("a kit server must not merge into a native one: %+v", sent.MCPServers["clash"])
	}
	var warnings []string
	for _, e := range rec.ofType(event.RuntimeStatus) {
		if d := e.Data.(event.RuntimeStatusData); d.Status == "warning" {
			warnings = append(warnings, d.Message)
		}
	}
	if len(warnings) != 2 || !strings.Contains(strings.Join(warnings, "|"), "HK_DEFINITELY_UNSET") || !strings.Contains(strings.Join(warnings, "|"), "already has a server") {
		t.Fatalf("warnings: %q", warnings)
	}
}

func TestMCPToolApprovalViaElicitation(t *testing.T) {
	decide := permission.Allow
	var got permission.Request
	rec := &recorder{decide: func(r permission.Request) permission.Decision {
		got = r
		return permission.Decision{Outcome: decide}
	}}
	s := startSession(t, newFake(t), agent.SessionConfig{}, rec)
	res, err := s.Prompt(context.Background(), agent.Turn{ID: "t1", Input: agent.Text("[mcp]")})
	if err != nil || res.Status != event.TurnStatusCompleted {
		t.Fatalf("%+v %v", res, err)
	}
	if got.Kind != permission.KindTool || got.Tool != "mcp/kit" || got.Title != "Allow kit to run echo?" || string(got.Input) != `{"text":"ping"}` || got.Supports(permission.AllowSession) {
		t.Fatalf("approval request: %+v", got)
	}
	if d := rec.ofType(event.ToolCompleted)[0].Data.(event.ToolCompletedData); d.Status != event.ToolStatusCompleted {
		t.Fatalf("tool: %+v", d)
	}
	decide = permission.Abort
	if res, _ := s.Prompt(context.Background(), agent.Turn{ID: "t2", Input: agent.Text("[mcp]")}); res.Status != event.TurnStatusCancelled {
		t.Fatalf("abort: %+v", res)
	}
}
