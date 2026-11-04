package acp

import (
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

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/internal/jsonrpc"
	"github.com/tomanagle/harness-kit/core/mcp"
	"github.com/tomanagle/harness-kit/core/permission"
)

// The test executable acts as a real newline-framed ACP subprocess. No network,
// installed agent, account, shell script, or model quota is needed.
func TestACPHelperProcess(t *testing.T) {
	if os.Getenv("HARNESS_ACP_HELPER") != "1" {
		return
	}
	mode := os.Getenv("HARNESS_ACP_SCENARIO")
	var conn *jsonrpc.Conn
	var mu sync.Mutex
	var activeCancel context.CancelFunc
	method := ""
	values := map[string]any{"model": "alpha", "effort": "low", "verbose": false}
	if mode == "sticky" {
		values["model"] = "beta"
		values["effort"] = "high"
	}
	options := func() []any {
		if mode == "no-config" {
			return []any{}
		}
		return []any{
			map[string]any{"id": "model", "name": "Model", "category": "model", "type": "select", "currentValue": values["model"], "options": []any{map[string]any{"group": "available", "options": []any{map[string]any{"value": "alpha", "name": "Alpha"}, map[string]any{"value": "beta", "name": "Beta"}}}}},
			map[string]any{"id": "effort", "name": "Reasoning", "category": "thought_level", "type": "select", "currentValue": values["effort"], "options": []any{map[string]any{"value": "low", "name": "Low"}, map[string]any{"value": "high", "name": "High"}}},
			map[string]any{"id": "verbose", "name": "Verbose", "type": "boolean", "currentValue": values["verbose"]},
		}
	}
	notify := func(update any) {
		_ = conn.Notify(context.Background(), "session/update", map[string]any{"sessionId": "native-session", "update": update})
	}
	message := func(text string) {
		notify(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}})
	}
	h := jsonrpc.HandlerFunc(func(ctx context.Context, req *jsonrpc.Request) (any, error) {
		switch req.Method {
		case "initialize":
			if mode == "hang-init" {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			var params struct {
				ProtocolVersion    int `json:"protocolVersion"`
				ClientCapabilities struct {
					FS       map[string]bool            `json:"fs"`
					Terminal bool                       `json:"terminal"`
					Session  map[string]json.RawMessage `json:"session"`
				} `json:"clientCapabilities"`
			}
			_ = req.UnmarshalParams(&params)
			if params.ProtocolVersion != 1 {
				return nil, fmt.Errorf("wrong version")
			}
			if strings.HasPrefix(mode, "compact") && string(params.ClientCapabilities.Session["compaction"]) != "{}" {
				return nil, fmt.Errorf("compaction notifications not negotiated")
			}
			if mode == "default-capabilities" && (len(params.ClientCapabilities.FS) > 0 || params.ClientCapabilities.Terminal) {
				return nil, fmt.Errorf("unsafe default capabilities")
			}
			version := 1
			if mode == "version" {
				version = 2
			}
			caps := map[string]any{"loadSession": true, "promptCapabilities": map[string]any{"image": mode == "images"}, "mcpCapabilities": map[string]any{"http": mode == "http"}}
			if mode == "resume" {
				caps["sessionCapabilities"] = map[string]any{"resume": map[string]any{}}
			}
			if mode == "no-resume" {
				caps["loadSession"] = false
			}
			return map[string]any{"protocolVersion": version, "agentCapabilities": caps, "agentInfo": map[string]any{"name": "mock", "version": "1.2.3"}, "authMethods": []any{map[string]any{"id": "mock-auth"}}}, nil
		case "authenticate":
			var params map[string]string
			_ = req.UnmarshalParams(&params)
			if params["methodId"] != "mock-auth" {
				return nil, fmt.Errorf("bad auth")
			}
			return map[string]any{}, nil
		case "session/new", "session/load", "session/resume":
			mu.Lock()
			method = req.Method
			result := map[string]any{"configOptions": options()}
			mu.Unlock()
			if req.Method == "session/new" {
				result["sessionId"] = "native-session"
			}
			if req.Method == "session/load" {
				message("old history must not replay")
			}
			if strings.HasPrefix(mode, "compact") {
				notify(map[string]any{"sessionUpdate": "available_commands_update", "availableCommands": []any{map[string]any{"name": "compact", "description": "Compact context"}}})
			}
			return result, nil
		case "session/set_config_option":
			var params struct {
				ID    string `json:"configId"`
				Value any    `json:"value"`
				Type  string `json:"type"`
			}
			_ = req.UnmarshalParams(&params)
			if params.ID == "verbose" && params.Type != "boolean" {
				return nil, fmt.Errorf("boolean type missing")
			}
			mu.Lock()
			values[params.ID] = params.Value
			result := map[string]any{"configOptions": options()}
			mu.Unlock()
			return result, nil
		case "session/cancel":
			mu.Lock()
			cancel := activeCancel
			mu.Unlock()
			if cancel != nil && mode != "stubborn" {
				cancel()
			}
			return nil, nil
		case "session/prompt":
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			mu.Lock()
			activeCancel = cancel
			mu.Unlock()
			var params struct {
				Prompt []struct{ Type, Text, Data, MimeType string } `json:"prompt"`
			}
			_ = req.UnmarshalParams(&params)
			text := ""
			for _, part := range params.Prompt {
				text += part.Text
				if part.Type == "image" {
					if part.Data == "" || part.MimeType != "image/png" {
						return nil, fmt.Errorf("invalid image")
					}
					text += "image"
				}
			}
			switch text {
			case "/compact":
				if !strings.HasPrefix(mode, "compact") {
					return nil, fmt.Errorf("unadvertised command was invoked")
				}
				notify(map[string]any{"sessionUpdate": "compaction_update", "compactionId": "cmp_1", "status": "in_progress"})
				switch mode {
				case "compact-missing":
				case "compact-failed":
					notify(map[string]any{"sessionUpdate": "compaction_update", "compactionId": "cmp_1", "status": "failed", "error": "provider unavailable"})
				case "compact-cancelled":
					notify(map[string]any{"sessionUpdate": "compaction_update", "compactionId": "cmp_1", "status": "cancelled"})
				case "compact-unknown":
					notify(map[string]any{"sessionUpdate": "compaction_update", "compactionId": "cmp_1", "status": "future-status"})
				default:
					notify(map[string]any{"sessionUpdate": "compaction_update", "compactionId": "cmp_1", "status": "completed"})
					if mode == "compact-wait" {
						<-ctx.Done()
						return map[string]any{"stopReason": "cancelled"}, nil
					}
					notify(map[string]any{"sessionUpdate": "usage_update", "used": 1200, "size": 200000})
				}
				message("Compacted")
				return map[string]any{"stopReason": "end_turn"}, nil
			case "wait":
				message("waiting")
				<-ctx.Done()
				return map[string]any{"stopReason": "cancelled"}, nil
			case "crash":
				os.Exit(9)
			case "permission":
				choices := []any{map[string]any{"optionId": "once", "name": "Allow", "kind": "allow_once"}, map[string]any{"optionId": "never", "name": "Deny", "kind": "reject_once"}, map[string]any{"optionId": "always", "name": "Always", "kind": "allow_always"}}
				var result struct {
					Outcome struct{ Outcome, OptionID string } `json:"outcome"`
				}
				err := conn.Call(ctx, "session/request_permission", map[string]any{"sessionId": "native-session", "toolCall": map[string]any{"toolCallId": "call", "name": "shell", "title": "Run tests", "kind": "execute", "rawInput": map[string]any{"command": "go test ./..."}}, "options": choices}, &result)
				if ctx.Err() != nil {
					return map[string]any{"stopReason": "cancelled"}, nil
				}
				if err != nil {
					return nil, err
				}
				message(result.Outcome.Outcome + ":" + result.Outcome.OptionID)
			case "fs":
				var result struct {
					Content string `json:"content"`
				}
				err := conn.Call(ctx, "fs/read_text_file", map[string]any{"sessionId": "native-session", "path": filepath.Join(os.TempDir(), "virtual.go"), "line": 1, "limit": 2}, &result)
				if err != nil {
					message(err.Error())
				} else {
					message(result.Content)
				}
			case "terminal":
				var result CreateTerminalResponse
				err := conn.Call(ctx, "terminal/create", map[string]any{"sessionId": "native-session", "command": "host-selected", "args": []string{"test"}}, &result)
				if err != nil {
					return nil, err
				}
				var output TerminalOutput
				if err = conn.Call(ctx, "terminal/output", TerminalRequest{"native-session", result.TerminalID}, &output); err != nil {
					return nil, err
				}
				message(output.Output)
			default:
				notify(map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "Thinking"}})
				notify(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "tool-1", "name": "read", "title": "Read file", "kind": "read", "status": "in_progress"})
				notify(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "tool-1", "status": "completed", "rawOutput": "contents"})
				notify(map[string]any{"sessionUpdate": "usage_update", "used": 40, "size": 1000, "cost": map[string]any{"amount": 0.01, "currency": "USD"}})
				mu.Lock()
				output := fmt.Sprintf("%s:%s:%v:%s:%s", values["model"], values["effort"], values["verbose"], method, text)
				mu.Unlock()
				message(output)
			}
			return map[string]any{"stopReason": "end_turn"}, nil
		default:
			return nil, jsonrpc.NewError(jsonrpc.CodeMethodNotFound, "unknown method", nil)
		}
	})
	conn = jsonrpc.NewConn(os.Stdin, os.Stdout, h, jsonrpc.Options{})
	_ = conn.Run(context.Background())
	os.Exit(0)
}

type captureHandler struct {
	mu       sync.Mutex
	events   []event.Event
	approval func(context.Context, permission.Request) (permission.Decision, error)
	signal   chan struct{}
}

func (h *captureHandler) Emit(e event.Event) {
	h.mu.Lock()
	h.events = append(h.events, e)
	h.mu.Unlock()
	if h.signal != nil {
		select {
		case h.signal <- struct{}{}:
		default:
		}
	}
}
func (h *captureHandler) RequestApproval(ctx context.Context, req permission.Request) (permission.Decision, error) {
	if h.approval != nil {
		return h.approval(ctx, req)
	}
	return permission.Decision{Outcome: permission.Deny}, nil
}
func (h *captureHandler) text() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out string
	for _, e := range h.events {
		if d, ok := e.Data.(event.MessageCompletedData); ok {
			out += d.Text
		}
	}
	return out
}
func mockRuntime(t *testing.T, mode string, opts ...Option) *Runtime {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	base := []Option{WithArgs("-test.run=^TestACPHelperProcess$"), WithEnv(map[string]string{"HARNESS_ACP_HELPER": "1", "HARNESS_ACP_SCENARIO": mode}), WithStartTimeout(3 * time.Second), WithCancelTimeout(150 * time.Millisecond)}
	return New(executable, append(base, opts...)...)
}
func startMock(t *testing.T, r *Runtime, cfg agent.SessionConfig, h *captureHandler) *Session {
	t.Helper()
	live, err := r.Start(context.Background(), cfg, h)
	if err != nil {
		t.Fatal(err)
	}
	s := live.(*Session)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}
func prompt(t *testing.T, s *Session, id, text string, settings *agent.TurnConfig) agent.TurnResult {
	t.Helper()
	result, err := s.Prompt(context.Background(), agent.Turn{ID: id, Input: agent.Text(text), Config: settings})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestLifecycleConfigurationAndUsage(t *testing.T) {
	r := mockRuntime(t, "default-capabilities")
	h := &captureHandler{}
	s := startMock(t, r, agent.SessionConfig{}, h)
	selected := agent.TurnConfig{Model: "beta", Effort: "high", Options: map[string]json.RawMessage{"config": json.RawMessage(`{"verbose":true}`)}}
	if got := prompt(t, s, "one", "hello", &selected); got.Status != event.TurnStatusCompleted {
		t.Fatal(got)
	}
	prompt(t, s, "two", "again", nil)
	if got := h.text(); got != "beta:high:true:session/new:helloalpha:low:false:session/new:again" {
		t.Fatal(got)
	}
	if defaults := s.NativeDefaults(); defaults.Model != "alpha" || defaults.Effort != "low" {
		t.Fatal(defaults)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := map[event.Type]int{}
	for _, e := range h.events {
		seen[e.Type]++
		if u, ok := e.Data.(event.UsageData); ok {
			if u.TotalTokens != 0 || u.ContextWindow != 1000 || u.CostUSD == nil || *u.CostUSD != 0.01 || u.Scope != "session" {
				t.Fatalf("incorrect usage semantics: %+v", u)
			}
		}
	}
	for _, kind := range []event.Type{event.MessageDelta, event.MessageCompleted, event.ReasoningDelta, event.ReasoningCompleted, event.ToolStarted, event.ToolCompleted, event.Usage} {
		if seen[kind] != 2 {
			t.Fatalf("%s: %d", kind, seen[kind])
		}
	}
}

func TestResumeNegotiationAndHistorySuppression(t *testing.T) {
	for _, mode := range []string{"load", "resume", "sticky"} {
		t.Run(mode, func(t *testing.T) {
			h := &captureHandler{}
			cfg := agent.SessionConfig{ResumeID: "native-session"}
			if mode == "sticky" {
				cfg.NativeDefaults = &agent.TurnConfig{Model: "alpha", Effort: "low", Options: map[string]json.RawMessage{"config": json.RawMessage(`{"model":"alpha","effort":"low","verbose":false}`)}}
			}
			s := startMock(t, mockRuntime(t, mode), cfg, h)
			if h.text() != "" {
				t.Fatal("native history duplicated")
			}
			prompt(t, s, "resume-turn", "hello", nil)
			method := "session/load"
			if mode == "resume" {
				method = "session/resume"
			}
			if !strings.Contains(h.text(), "alpha:low:false:"+method) {
				t.Fatal(h.text())
			}
		})
	}
	_, err := mockRuntime(t, "no-resume").Start(context.Background(), agent.SessionConfig{ResumeID: "native-session"}, &captureHandler{})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Fatal(err)
	}
}

func TestApprovalOutcomes(t *testing.T) {
	for _, outcome := range []permission.Outcome{permission.Allow, permission.Deny, permission.AllowSession, permission.Abort} {
		t.Run(string(outcome), func(t *testing.T) {
			h := &captureHandler{approval: func(ctx context.Context, req permission.Request) (permission.Decision, error) {
				if req.Supports(permission.AllowSession) || req.Kind != permission.KindCommand || req.Command != "go test ./..." {
					t.Errorf("bad approval: %+v", req)
				}
				return permission.Decision{Outcome: outcome}, nil
			}}
			s := startMock(t, mockRuntime(t, ""), agent.SessionConfig{}, h)
			result := prompt(t, s, "approve", "permission", nil)
			if outcome == permission.Abort {
				if result.Status != event.TurnStatusCancelled {
					t.Fatal(result)
				}
				return
			}
			expect := map[permission.Outcome]string{permission.Allow: "selected:once", permission.Deny: "selected:never", permission.AllowSession: "cancelled:"}[outcome]
			if h.text() != expect {
				t.Fatal(h.text())
			}
		})
	}
}

func TestCancellationAndConcurrentPrompt(t *testing.T) {
	for _, mode := range []string{"cooperative", "stubborn"} {
		t.Run(mode, func(t *testing.T) {
			h := &captureHandler{signal: make(chan struct{}, 10)}
			s := startMock(t, mockRuntime(t, mode), agent.SessionConfig{}, h)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				result, err := s.Prompt(ctx, agent.Turn{ID: "wait", Input: agent.Text("wait")})
				if result.Status != event.TurnStatusCancelled && err == nil {
					err = fmt.Errorf("unexpected result: %+v", result)
				}
				done <- err
			}()
			select {
			case <-h.signal:
			case <-time.After(3 * time.Second):
				t.Fatal("prompt did not start")
			}
			if _, err := s.Prompt(context.Background(), agent.Turn{}); err == nil {
				t.Fatal("concurrent prompt accepted")
			}
			cancel()
			select {
			case err := <-done:
				if mode == "stubborn" && !errors.Is(err, agent.ErrSessionDead) {
					t.Fatal(err)
				}
				if mode == "cooperative" && err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancellation hung")
			}
			if mode == "cooperative" {
				prompt(t, s, "after", "after", nil)
			} else {
				select {
				case <-s.exited:
				case <-time.After(time.Second):
					t.Fatal("process leaked")
				}
			}
		})
	}
}

func TestCancellationUnblocksPermission(t *testing.T) {
	asked := make(chan struct{})
	h := &captureHandler{approval: func(ctx context.Context, _ permission.Request) (permission.Decision, error) {
		close(asked)
		<-ctx.Done()
		return permission.Decision{}, ctx.Err()
	}}
	s := startMock(t, mockRuntime(t, ""), agent.SessionConfig{}, h)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.Prompt(ctx, agent.Turn{ID: "approve", Input: agent.Text("permission")})
		done <- err
	}()
	select {
	case <-asked:
	case <-time.After(3 * time.Second):
		t.Fatal("approval not received")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("approval cancellation hung")
	}
}

func TestDiscoveryAndFailures(t *testing.T) {
	r := mockRuntime(t, "")
	d, err := r.Discover(context.Background())
	if err != nil || !d.Ready || d.Version != "1.2.3" || !d.Capabilities.Resume || d.Auth.State != agent.AuthUnknown {
		t.Fatalf("%+v %v", d, err)
	}
	for _, test := range []struct {
		name, mode string
		cfg        agent.SessionConfig
		want       error
	}{
		{"version", "version", agent.SessionConfig{}, agent.ErrUnsupported},
		{"instructions", "", agent.SessionConfig{Instructions: "system"}, agent.ErrUnsupported},
		{"model", "no-config", agent.SessionConfig{Model: "x"}, agent.ErrUnsupported},
		{"option", "", agent.SessionConfig{Options: map[string]any{"typo": true}}, agent.ErrInvalidConfig},
		{"choice", "", agent.SessionConfig{Model: "missing"}, agent.ErrInvalidConfig},
		{"auth", "", agent.SessionConfig{Options: Options{AuthMethod: "missing"}}, agent.ErrUnsupported},
		{"mcp-http", "", agent.SessionConfig{MCP: &mcp.Config{Servers: map[string]mcp.Server{"remote": {Type: "http", URL: "https://example.com/mcp"}}}}, agent.ErrUnsupported},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := mockRuntime(t, test.mode).Start(context.Background(), test.cfg, &captureHandler{})
			if !errors.Is(err, test.want) {
				t.Fatal(err)
			}
		})
	}
	s := startMock(t, r, agent.SessionConfig{Options: Options{AuthMethod: "mock-auth"}}, &captureHandler{})
	_, err = s.Prompt(context.Background(), agent.Turn{Input: agent.Text("crash")})
	if !errors.Is(err, agent.ErrSessionDead) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = mockRuntime(t, "hang-init").Start(ctx, agent.SessionConfig{}, &captureHandler{})
	if err == nil {
		t.Fatal("startup timeout ignored")
	}
}

func TestFileSystemIsOptIn(t *testing.T) {
	h := &captureHandler{}
	s := startMock(t, mockRuntime(t, "default-capabilities"), agent.SessionConfig{}, h)
	prompt(t, s, "fs", "fs", nil)
	if !strings.Contains(h.text(), "-32601") {
		t.Fatal(h.text())
	}
	called := false
	fs := FileSystem{ReadTextFile: func(ctx context.Context, req ReadTextFileRequest) (string, error) {
		called = true
		if req.Root == "" || req.Line == nil || *req.Line != 1 {
			t.Errorf("bad request: %+v", req)
		}
		return "unsaved editor content", nil
	}}
	h2 := &captureHandler{}
	s2 := startMock(t, mockRuntime(t, "", WithFileSystem(fs)), agent.SessionConfig{}, h2)
	prompt(t, s2, "read", "fs", nil)
	if !called || h2.text() != "unsaved editor content" {
		t.Fatal(h2.text())
	}
}

type mockTerminal struct{ released bool }

func (h *mockTerminal) Create(context.Context, CreateTerminalRequest) (CreateTerminalResponse, error) {
	return CreateTerminalResponse{TerminalID: "terminal"}, nil
}
func (h *mockTerminal) Output(context.Context, TerminalRequest) (TerminalOutput, error) {
	return TerminalOutput{Output: "terminal output"}, nil
}
func (h *mockTerminal) WaitForExit(context.Context, TerminalRequest) (TerminalExitStatus, error) {
	return TerminalExitStatus{}, nil
}
func (h *mockTerminal) Kill(context.Context, TerminalRequest) error    { return nil }
func (h *mockTerminal) Release(context.Context, TerminalRequest) error { return nil }
func (h *mockTerminal) ReleaseSession(context.Context, string) error   { h.released = true; return nil }

func TestTerminalHostCleanup(t *testing.T) {
	host := &mockTerminal{}
	h := &captureHandler{}
	s := startMock(t, mockRuntime(t, "", WithTerminal(host)), agent.SessionConfig{}, h)
	prompt(t, s, "terminal", "terminal", nil)
	if h.text() != "terminal output" {
		t.Fatal(h.text())
	}
	_ = s.Close(context.Background())
	if !host.released {
		t.Fatal("host terminal resources leaked")
	}
}

func TestImageCapabilityAndLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image.png")
	if err := os.WriteFile(path, append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...), 0600); err != nil {
		t.Fatal(err)
	}
	input := []agent.InputPart{{Type: "image", Path: path}}
	for _, scenario := range []struct {
		mode  string
		limit int64
		want  error
	}{{"", 100, agent.ErrUnsupported}, {"images", 4, agent.ErrInvalidConfig}, {"images", 100, nil}} {
		t.Run(fmt.Sprint(scenario.mode, scenario.limit), func(t *testing.T) {
			s := startMock(t, mockRuntime(t, scenario.mode, WithMaxImageBytes(scenario.limit)), agent.SessionConfig{}, &captureHandler{})
			_, err := s.Prompt(context.Background(), agent.Turn{Input: input})
			if !errors.Is(err, scenario.want) {
				t.Fatal(err)
			}
		})
	}
}

func TestRetainedOutputLimitPreservesStream(t *testing.T) {
	h := &captureHandler{}
	s := startMock(t, mockRuntime(t, "", WithMaxRetainedOutput(4)), agent.SessionConfig{}, h)
	prompt(t, s, "limited", "long response", nil)
	h.mu.Lock()
	defer h.mu.Unlock()
	var stream string
	truncated := false
	for _, e := range h.events {
		switch data := e.Data.(type) {
		case event.MessageDeltaData:
			stream += data.Text
		case event.MessageCompletedData:
			if len(data.Text) > 4 {
				t.Fatalf("snapshot exceeded limit: %q", data.Text)
			}
		case event.ToolCompletedData:
			if len(data.Output) > 4 {
				t.Fatalf("tool exceeded limit: %q", data.Output)
			}
		case event.RuntimeStatusData:
			truncated = truncated || data.Status == "output_truncated"
		}
	}
	if !strings.Contains(stream, "long response") || !truncated {
		t.Fatalf("stream=%q truncation=%v", stream, truncated)
	}
	if got := bounded("αβγ", 3); got != "α" {
		t.Fatalf("invalid UTF-8 truncation: %q", got)
	}
}

func TestLiveACP(t *testing.T) {
	command := os.Getenv("HARNESS_KIT_TEST_ACP_COMMAND")
	if command == "" {
		t.Skip("set HARNESS_KIT_TEST_ACP_COMMAND to opt into paid live inference")
	}
	var args []string
	if raw := os.Getenv("HARNESS_KIT_TEST_ACP_ARGS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r := New(command, WithArgs(args...))
	h := &captureHandler{}
	s, err := r.Start(ctx, agent.SessionConfig{Cwd: t.TempDir()}, h)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	result, err := s.Prompt(ctx, agent.Turn{ID: "live", Input: agent.Text("Reply with exactly ACP_OK. Do not use tools.")})
	if err != nil || result.Status != event.TurnStatusCompleted || !strings.Contains(h.text(), "ACP_OK") {
		t.Fatalf("result=%+v err=%v text=%q", result, err, h.text())
	}
}
