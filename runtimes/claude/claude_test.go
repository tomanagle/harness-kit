package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/agent"
	"github.com/tomanagle/harness-kit/event"
	"github.com/tomanagle/harness-kit/mcp"
	"github.com/tomanagle/harness-kit/permission"
)

// The test binary doubles as a scripted fake `claude` executable.
func TestMain(m *testing.M) {
	if os.Getenv("HK_FAKE_CLAUDE") == "1" {
		os.Exit(fakeClaude(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func flag(args []string, name string) (string, bool) {
	i := slices.Index(args, name)
	if i < 0 || i+1 >= len(args) {
		return "", i >= 0
	}
	return args[i+1], true
}

func fakeClaude(args []string) int {
	if len(args) > 0 && args[0] == "--version" {
		fmt.Println(os.Getenv("HK_FAKE_CLAUDE_VERSION") + " (Claude Code)")
		return 0
	}
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		if os.Getenv("HK_FAKE_CLAUDE_AUTH") == "none" {
			fmt.Println(`{"loggedIn":false,"authMethod":"none","apiProvider":"firstParty"}`)
			return 1
		}
		fmt.Println(`{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","email":"user@example.com","subscriptionType":"max"}`)
		return 0
	}
	if log := os.Getenv("HK_FAKE_CLAUDE_ARGS"); log != "" {
		f, _ := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		b, _ := json.Marshal(args)
		f.Write(append(b, '\n'))
		f.Close()
	}
	if f, _ := flag(args, "--output-format"); f != "stream-json" || !slices.Contains(args, "--verbose") || !slices.Contains(args, "-p") {
		fmt.Fprintln(os.Stderr, "fake claude: bad flags", args)
		return 64
	}
	promptBytes, _ := io.ReadAll(os.Stdin)
	if path := os.Getenv("HK_FAKE_CLAUDE_INPUT"); path != "" {
		if err := os.WriteFile(path, promptBytes, 0o600); err != nil {
			return 65
		}
	}
	prompt := string(promptBytes)

	sid, _ := flag(args, "--session-id")
	if r, ok := flag(args, "--resume"); ok {
		sid = r
		if slices.Contains(args, "--fork-session") {
			sid = "forked-" + r
		}
	}
	out := bufio.NewWriter(os.Stdout)
	emit := func(v any) {
		b, _ := json.Marshal(v)
		out.Write(append(b, '\n'))
		out.Flush()
	}
	if strings.Contains(prompt, "[crash]") {
		fmt.Fprintln(os.Stderr, "fatal: something broke")
		return 2
	}
	if strings.Contains(prompt, "[authfail]") {
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": "Not logged in · Please run /login", "session_id": sid, "total_cost_usd": 0, "usage": map[string]any{}})
		return 1
	}
	emit(map[string]any{"type": "system", "subtype": "init", "session_id": sid, "model": "claude-test", "permissionMode": "default", "tools": []string{"Bash"}})
	emit(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{"status": "allowed"}, "session_id": sid})
	textBlock := func(msgID, text string) {
		emit(map[string]any{"type": "stream_event", "session_id": sid, "parent_tool_use_id": nil, "event": map[string]any{"type": "message_start", "message": map[string]any{"id": msgID}}})
		emit(map[string]any{"type": "stream_event", "session_id": sid, "event": map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "text"}}})
		for _, w := range strings.SplitAfter(text, " ") {
			emit(map[string]any{"type": "stream_event", "session_id": sid, "event": map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "text_delta", "text": w}}})
		}
		emit(map[string]any{"type": "assistant", "session_id": sid, "parent_tool_use_id": nil, "message": map[string]any{"id": msgID, "role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}})
		emit(map[string]any{"type": "stream_event", "session_id": sid, "event": map[string]any{"type": "content_block_stop", "index": 1}})
	}

	if strings.Contains(prompt, "[slow]") {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, os.Interrupt)
		textBlock("msg_slow", "working on it")
		select {
		case <-sigs:
			emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "session_id": sid, "usage": map[string]any{}})
			return 130
		case <-time.After(20 * time.Second):
			return 0
		}
	}
	if strings.Contains(prompt, "[ignore-int]") {
		signal.Ignore(os.Interrupt)
		textBlock("msg_stubborn", "not stopping")
		time.Sleep(30 * time.Second)
		return 0
	}
	if strings.Contains(prompt, "[tool]") {
		emit(map[string]any{"type": "assistant", "session_id": sid, "message": map[string]any{"id": "msg_tool", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "ls", "description": "list"}}}}})
		emit(map[string]any{"type": "user", "session_id": sid, "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": []any{map[string]any{"type": "text", "text": "file.txt\n"}}, "is_error": false}}}})
		emit(map[string]any{"type": "assistant", "session_id": sid, "message": map[string]any{"id": "msg_tool2", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_2", "name": "Write", "input": map[string]any{"file_path": "/x"}}}}})
		emit(map[string]any{"type": "system", "subtype": "permission_denied", "session_id": sid, "tool_name": "Write"})
		emit(map[string]any{"type": "user", "session_id": sid, "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_2", "content": "Permission denied", "is_error": true}}}})
	}
	if strings.Contains(prompt, "[maxturns]") {
		emit(map[string]any{"type": "result", "subtype": "error_max_turns", "is_error": true, "session_id": sid, "total_cost_usd": 0.5, "usage": map[string]any{"input_tokens": 1}})
		return 1
	}
	textBlock("msg_1", "Hello from claude")
	cost := 0.25
	if strings.Contains(prompt, "second") {
		cost = 0.40
	}
	emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "Hello from claude", "session_id": sid, "total_cost_usd": cost,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "cache_read_input_tokens": 100, "cache_creation_input_tokens": 50}})
	return 0
}

type recorder struct {
	mu     sync.Mutex
	events []event.Event
}

func (r *recorder) Emit(ev event.Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recorder) RequestApproval(context.Context, permission.Request) (permission.Decision, error) {
	return permission.Decision{Outcome: permission.Deny}, nil
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

func newFake(t *testing.T, version string, env ...string) (*Runtime, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argLog := filepath.Join(t.TempDir(), "args.jsonl")
	env = append([]string{"HK_FAKE_CLAUDE=1", "GORACE=atexit_sleep_ms=0", "HK_FAKE_CLAUDE_VERSION=" + version, "HK_FAKE_CLAUDE_ARGS=" + argLog}, env...)
	return New(WithPath(exe), WithEnv(env...), WithInterruptGrace(500*time.Millisecond)), argLog
}

func invocations(t *testing.T, path string) [][]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var a []string
		json.Unmarshal([]byte(line), &a)
		out = append(out, a)
	}
	return out
}

func TestDiscover(t *testing.T) {
	rt, _ := newFake(t, "2.1.316")
	d, err := rt.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !d.Installed || d.Version != "2.1.316" || d.Auth.State != agent.AuthAuthenticated || d.Auth.Method != "claude.ai" || d.Auth.Plan != "max" || !d.Ready {
		t.Fatalf("discovery: %+v", d)
	}
	if b, _ := json.Marshal(d); strings.Contains(string(b), "user@example.com") {
		t.Fatal("discovery must not leak the account email")
	}

	rt, _ = newFake(t, "2.1.316", "HK_FAKE_CLAUDE_AUTH=none")
	d, _ = rt.Discover(context.Background())
	if d.Auth.State != agent.AuthUnauthenticated || d.Ready {
		t.Fatalf("signed out: %+v", d)
	}
	d, _ = New(WithPath("/definitely/not/claude")).Discover(context.Background())
	if d.Installed || d.Ready {
		t.Fatalf("missing: %+v", d)
	}
}

func TestTurnsStreamAndResume(t *testing.T) {
	rt, argLog := newFake(t, "2.1.316")
	rec := &recorder{}
	tools := []string{"Read", "Bash"}
	sess, err := rt.Start(context.Background(), agent.SessionConfig{
		Cwd: t.TempDir(), Model: "sonnet", Instructions: "be brief",
		Options: Options{PermissionMode: "acceptEdits", AllowedTools: []string{"Read", "Bash(git *)"}, Tools: &tools, MaxTurns: 3},
	}, rec)
	if err != nil {
		t.Fatal(err)
	}
	id := sess.RuntimeSessionID()
	if len(id) != 36 {
		t.Fatalf("expected a UUID session id, got %q", id)
	}

	res, err := sess.Prompt(context.Background(), agent.Turn{ID: "t1", Input: agent.Text("hello [tool]")})
	if err != nil || res.Status != event.TurnStatusCompleted {
		t.Fatalf("turn 1: %+v %v", res, err)
	}
	if res.Usage == nil || res.Usage.InputTokens != 10 || res.Usage.CachedInputTokens != 100 || res.Usage.CostUSD == nil || *res.Usage.CostUSD != 0.25 {
		t.Fatalf("usage: %+v", res.Usage)
	}
	var text string
	var itemID string
	for _, e := range rec.ofType(event.MessageDelta) {
		d := e.Data.(event.MessageDeltaData)
		text += d.Text
		itemID = d.ItemID
	}
	if text != "Hello from claude" {
		t.Fatalf("streamed %q", text)
	}
	mc := rec.ofType(event.MessageCompleted)
	if len(mc) != 1 || mc[0].Data.(event.MessageCompletedData).ItemID != itemID {
		t.Fatalf("message completed must share the delta item id %q: %+v", itemID, mc)
	}
	started := rec.ofType(event.ToolStarted)
	if len(started) != 2 || started[0].Data.(event.ToolStartedData).Kind != event.ToolKindCommand || started[0].Data.(event.ToolStartedData).Title != "Bash: ls" {
		t.Fatalf("tool started: %+v", started)
	}
	done := rec.ofType(event.ToolCompleted)
	if len(done) != 2 || done[0].Data.(event.ToolCompletedData).Output != "file.txt\n" || done[1].Data.(event.ToolCompletedData).Status != event.ToolStatusFailed {
		t.Fatalf("tool completed: %+v", done)
	}
	if st := rec.ofType(event.RuntimeStatus); len(st) < 2 {
		t.Fatalf("expected ready and permission_denied status events: %+v", st)
	}

	res, err = sess.Prompt(context.Background(), agent.Turn{ID: "t2", Input: agent.Text("second turn")})
	if err != nil || res.Usage.CostUSD == nil || fmt.Sprintf("%.2f", *res.Usage.CostUSD) != "0.15" {
		t.Fatalf("turn 2 cost delta: %+v %v", res.Usage, err)
	}

	calls := invocations(t, argLog)
	if len(calls) != 2 {
		t.Fatalf("invocations: %v", calls)
	}
	first, second := calls[0], calls[1]
	if v, _ := flag(first, "--session-id"); v != id {
		t.Fatalf("first turn must pin the session id: %v", first)
	}
	if v, _ := flag(second, "--resume"); v != id || slices.Contains(second, "--session-id") {
		t.Fatalf("second turn must resume: %v", second)
	}
	for _, want := range [][2]string{
		{"--model", "sonnet"}, {"--append-system-prompt", "be brief"}, {"--permission-mode", "acceptEdits"},
		{"--allowedTools", "Read,Bash(git *)"}, {"--tools", "Read,Bash"}, {"--max-turns", "3"}, {"--permission-prompts", "none"},
	} {
		if v, _ := flag(first, want[0]); v != want[1] {
			t.Errorf("%s = %q, want %q (args %v)", want[0], v, want[1], first)
		}
	}
	if !slices.Contains(first, "--include-partial-messages") {
		t.Error("partial messages should be on by default")
	}
	for _, a := range first {
		if strings.Contains(a, "hello") {
			t.Fatal("the prompt must be passed on stdin, not argv")
		}
	}
}

func TestOldVersionSkipsPermissionPromptsFlag(t *testing.T) {
	rt, argLog := newFake(t, "2.1.100")
	sess, err := rt.Start(context.Background(), agent.SessionConfig{}, &recorder{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Prompt(context.Background(), agent.Turn{ID: "t", Input: agent.Text("hi")}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(invocations(t, argLog)[0], "--permission-prompts") {
		t.Fatal("flag must not be passed to versions that reject it")
	}
}

func TestResumeAndFork(t *testing.T) {
	rt, argLog := newFake(t, "2.1.316")
	sess, err := rt.Start(context.Background(), agent.SessionConfig{ResumeID: "abc", Options: map[string]any{"forkOnResume": true}}, &recorder{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Prompt(context.Background(), agent.Turn{ID: "t1", Input: agent.Text("hi")}); err != nil {
		t.Fatal(err)
	}
	if sess.RuntimeSessionID() != "forked-abc" {
		t.Fatalf("fork should adopt the new session id: %q", sess.RuntimeSessionID())
	}
	res, err := sess.Prompt(context.Background(), agent.Turn{ID: "t2", Input: agent.Text("second")})
	if err != nil {
		t.Fatal(err)
	}
	calls := invocations(t, argLog)
	if !slices.Contains(calls[0], "--fork-session") || slices.Contains(calls[1], "--fork-session") {
		t.Fatalf("fork only on the first resumed turn: %v", calls)
	}
	if v, _ := flag(calls[1], "--resume"); v != "forked-abc" {
		t.Fatalf("second turn should resume the fork: %v", calls[1])
	}
	// The resumed session had no cost baseline on turn 1, so only turn 2
	// reports a per-turn cost.
	if res.Usage.CostUSD == nil {
		t.Fatal("turn 2 should have a cost delta")
	}
}

func TestCancelSendsInterrupt(t *testing.T) {
	rt, _ := newFake(t, "2.1.316")
	rec := &recorder{}
	sess, _ := rt.Start(context.Background(), agent.SessionConfig{}, rec)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for len(rec.ofType(event.MessageCompleted)) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	res, err := sess.Prompt(ctx, agent.Turn{ID: "t", Input: agent.Text("[slow]")})
	if err != nil || res.Status != event.TurnStatusCancelled {
		t.Fatalf("%+v %v", res, err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("SIGINT should end the turn promptly")
	}

	// A process that ignores SIGINT is killed after the grace period.
	ctx, cancel = context.WithCancel(context.Background())
	rec2 := &recorder{}
	sess2, _ := rt.Start(context.Background(), agent.SessionConfig{}, rec2)
	go func() {
		for len(rec2.ofType(event.MessageCompleted)) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	start = time.Now()
	res, _ = sess2.Prompt(ctx, agent.Turn{ID: "t", Input: agent.Text("[ignore-int]")})
	if res.Status != event.TurnStatusCancelled || time.Since(start) > 5*time.Second {
		t.Fatalf("kill fallback: %+v after %s", res, time.Since(start))
	}
}

func TestFailures(t *testing.T) {
	rt, _ := newFake(t, "2.1.316")
	sess, _ := rt.Start(context.Background(), agent.SessionConfig{}, &recorder{})

	res, err := sess.Prompt(context.Background(), agent.Turn{ID: "t1", Input: agent.Text("[authfail]")})
	if err != nil || res.Status != event.TurnStatusFailed || !strings.Contains(res.Error.Message, "Not logged in") {
		t.Fatalf("auth failure: %+v %v", res, err)
	}
	res, err = sess.Prompt(context.Background(), agent.Turn{ID: "t2", Input: agent.Text("[maxturns]")})
	if err != nil || res.Status != event.TurnStatusFailed || res.Error.Code != "claude_error_max_turns" {
		t.Fatalf("max turns: %+v %v", res, err)
	}
	_, err = sess.Prompt(context.Background(), agent.Turn{ID: "t3", Input: agent.Text("[crash]")})
	if err == nil || !strings.Contains(err.Error(), "something broke") {
		t.Fatalf("crash should surface stderr: %v", err)
	}
	if _, err := sess.Prompt(context.Background(), agent.Turn{ID: "t4", Input: []agent.InputPart{{Type: "image", Path: "/x.png"}}}); err == nil {
		t.Fatal("missing image should fail before starting the process")
	}
	// A crashed turn does not kill the session: each turn is a new process.
	if res, err := sess.Prompt(context.Background(), agent.Turn{ID: "t5", Input: agent.Text("hi")}); err != nil || res.Status != event.TurnStatusCompleted {
		t.Fatalf("after crash: %+v %v", res, err)
	}
}

func TestOptionsValidationAndGuard(t *testing.T) {
	rt, _ := newFake(t, "2.1.316")
	if _, err := rt.Start(context.Background(), agent.SessionConfig{Options: map[string]any{"permissionMode": "yolo"}}, &recorder{}); err == nil {
		t.Fatal("invalid permission mode should fail")
	}
	if _, err := rt.Start(context.Background(), agent.SessionConfig{Options: map[string]any{"unknownOption": true}}, &recorder{}); err == nil {
		t.Fatal("unknown option should fail")
	}
	if rt.Unsafe(agent.SessionConfig{Options: map[string]any{"permissionMode": "bypassPermissions"}}) == "" {
		t.Fatal("bypassPermissions must be flagged")
	}
	if rt.Unsafe(agent.SessionConfig{Options: map[string]any{"permissionMode": "acceptEdits"}}) != "" {
		t.Fatal("acceptEdits is not unsafe")
	}
}

func TestMCPFlags(t *testing.T) {
	rt, argLog := newFake(t, "2.1.316", "HK_TEST_TOKEN=s3cret")
	no := false
	rec := &recorder{}
	sess, err := rt.Start(context.Background(), agent.SessionConfig{
		Options: Options{AllowedTools: []string{"Read"}},
		MCP: &mcp.Config{InheritNative: &no, Servers: map[string]mcp.Server{
			"kit":     {Command: "kit-server", Env: map[string]string{"T": "${HK_TEST_TOKEN}"}, Tools: mcp.ToolFilter{Allow: []string{"echo"}, Deny: []string{"rm"}}, Cwd: "/x"},
			"web":     {Type: "http", URL: "https://mcp.example.com/mcp", Headers: map[string]string{"Authorization": "Bearer ${HK_TEST_TOKEN}"}},
			"asky":    {Command: "a", Approval: mcp.ApprovalAsk},
			"missing": {Command: "x", Env: map[string]string{"K": "${HK_DEFINITELY_UNSET}"}},
		}},
	}, rec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close(context.Background()) })
	if _, err := sess.Prompt(context.Background(), agent.Turn{ID: "t", Input: agent.Text("hi")}); err != nil {
		t.Fatal(err)
	}
	args := invocations(t, argLog)[0]
	path, _ := flag(args, "--mcp-config")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("--mcp-config must name a file: %v", err)
	}
	if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("mcp config file mode %v", fi.Mode().Perm())
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("--mcp-config: %v %q", err, raw)
	}
	if strings.Contains(strings.Join(args, " "), "s3cret") {
		t.Fatal("secrets must not appear on the command line")
	}
	if cfg.MCPServers["kit"]["env"].(map[string]any)["T"] != "${HK_TEST_TOKEN}" || cfg.MCPServers["web"]["type"] != "http" {
		t.Fatalf("servers: %+v", cfg.MCPServers)
	}
	if _, ok := cfg.MCPServers["missing"]; ok {
		t.Fatal("server with an unset variable must be skipped")
	}
	if !slices.Contains(args, "--strict-mcp-config") {
		t.Fatal("inheritNative=false must pass --strict-mcp-config")
	}
	if v, _ := flag(args, "--allowedTools"); v != "Read,mcp__kit__echo,mcp__web__*" {
		t.Fatalf("--allowedTools = %q", v)
	}
	if v, _ := flag(args, "--disallowedTools"); v != "mcp__kit__rm" {
		t.Fatalf("--disallowedTools = %q", v)
	}
	var warnings []string
	for _, e := range rec.ofType(event.RuntimeStatus) {
		if d := e.Data.(event.RuntimeStatusData); d.Status == "warning" {
			warnings = append(warnings, d.Message)
		}
	}
	joined := strings.Join(warnings, "|")
	if len(warnings) != 3 || !strings.Contains(joined, "HK_DEFINITELY_UNSET") || !strings.Contains(joined, "cwd is not supported") || !strings.Contains(joined, `"ask"`) {
		t.Fatalf("warnings: %q", warnings)
	}
	sess.Close(context.Background())
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("Close must remove the MCP config file")
	}
}

func TestPermissionPromptTool(t *testing.T) {
	rt, argLog := newFake(t, "2.1.316")
	if !rt.UsesPermissionTool(agent.SessionConfig{}) || rt.UsesPermissionTool(agent.SessionConfig{DisableHostTools: true}) {
		t.Fatal("UsesPermissionTool")
	}
	rec := &recorder{}
	sess, err := rt.Start(context.Background(), agent.SessionConfig{
		PermissionTool: "harness_kit/approval_prompt",
		MCP:            &mcp.Config{Servers: map[string]mcp.Server{"asky": {Command: "a", Approval: mcp.ApprovalAsk}}},
	}, rec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close(context.Background()) })
	sess.Prompt(context.Background(), agent.Turn{ID: "t", Input: agent.Text("hi")})
	args := invocations(t, argLog)[0]
	if v, _ := flag(args, "--permission-prompt-tool"); v != "mcp__harness_kit__approval_prompt" {
		t.Fatalf("--permission-prompt-tool = %q", v)
	}
	if slices.Contains(args, "--permission-prompts") {
		t.Fatal("prompts must go to the bridge, not be auto-denied")
	}
	if v, _ := flag(args, "--allowedTools"); strings.Contains(v, "mcp__asky") {
		t.Fatal("approval ask servers must prompt (no allow rule)")
	}
	for _, e := range rec.ofType(event.RuntimeStatus) {
		if d := e.Data.(event.RuntimeStatusData); d.Status == "warning" {
			t.Fatalf("no warnings expected with the bridge: %+v", d)
		}
	}
	sess.Close(context.Background())
}
