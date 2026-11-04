package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/rpc/client"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
)

func TestMain(m *testing.M) {
	// Never touch the developer's real session history.
	store, err := os.MkdirTemp("", "hk-store-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HARNESS_KIT_STORE", store)
	os.Setenv("HARNESS_KIT_CONFIG", filepath.Join(store, "config", "config.json"))
	code := m.Run()
	os.RemoveAll(store)
	if builtBin != "" {
		os.RemoveAll(filepath.Dir(builtBin))
	}
	os.Exit(code)
}

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// binary builds the real harness-kit binary once per test run.
func binary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a binary")
	}
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "hk-bin-")
		if err != nil {
			buildErr = err
			return
		}
		builtBin = filepath.Join(dir, "harness-kit")
		if runtime.GOOS == "windows" {
			builtBin += ".exe"
		}
		out, err := exec.Command("go", "build", "-o", builtBin, ".").CombinedOutput()
		if err != nil {
			buildErr = errors.New(string(out))
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return builtBin
}

// TestSidecarProcess drives the real binary as a sidecar, exactly as
// Electron or a Wails backend would.
func TestSidecarProcess(t *testing.T) {
	bin := binary(t)

	events := make(chan event.Event, 1000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := client.Spawn(ctx, bin, []string{"serve", "--runtimes", "", "--fake-runtime", "--log-level", "warn"}, nil,
		client.Options{OnEvent: func(ev event.Event) { events <- ev }})
	if err != nil {
		t.Fatal(err)
	}
	if info := c.Info(); len(info.Capabilities.Runtimes) != 1 || info.Capabilities.Runtimes[0].Name != "fake" {
		t.Fatalf("runtimes: %+v", info.Capabilities.Runtimes)
	}
	sess, err := c.CreateSession(ctx, protocol.SessionCreateParams{Runtime: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Prompt(ctx, protocol.SessionPromptParams{SessionID: sess.ID, Input: agent.Text("[slow] long")}); err != nil {
		t.Fatal(err)
	}
	for ev := range events {
		if ev.Type == event.MessageDelta {
			break
		}
	}
	// Closing while a turn runs must shut down cleanly and promptly.
	start := time.Now()
	if err := c.Close(); err != nil {
		t.Fatalf("sidecar exit: %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("sidecar shutdown too slow")
	}
}

func TestSidecarExitsOnStdinEOF(t *testing.T) {
	bin := binary(t)
	cmd := exec.Command(bin, "--runtimes", "", "--fake-runtime", "--log-level", "error")
	stdin, _ := cmd.StdinPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stdin.Close() // simulates the parent going away
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("sidecar did not exit on stdin EOF")
	}
}

// run executes the CLI with stdin from a pipe (not a terminal).
func run(t *testing.T, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(binary(t), args...)
	cmd.Stdin = strings.NewReader(stdin)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		code = ee.ExitCode()
	case err != nil:
		t.Fatal(err)
	}
	return o.String(), e.String(), code
}

func TestCLIRuntimes(t *testing.T) {
	for _, mode := range [][]string{nil, {"--rpc"}} {
		out, errOut, code := run(t, "", append([]string{"runtimes", "list", "--runtimes", "", "--fake-runtime", "--json"}, mode...)...)
		var infos []map[string]any
		if code != 0 || json.Unmarshal([]byte(out), &infos) != nil || len(infos) != 1 || infos[0]["name"] != "fake" {
			t.Fatalf("runtimes list %v: code %d out %q err %q", mode, code, out, errOut)
		}
		out, _, code = run(t, "", append([]string{"runtimes", "discover", "--runtimes", "", "--fake-runtime"}, mode...)...)
		if code != 0 || !strings.Contains(out, "fake") || !strings.Contains(out, "authenticated") {
			t.Fatalf("discover %v: %d %q", mode, code, out)
		}
	}
	if _, errOut, code := run(t, "", "runtimes", "bogus"); code != 1 || !strings.Contains(errOut, "unknown command") {
		t.Fatalf("bad subcommand: %d %q", code, errOut)
	}
}

func TestCLIPrompt(t *testing.T) {
	for _, mode := range [][]string{nil, {"-rpc"}} {
		// Flags may follow the prompt words.
		out, errOut, code := run(t, "", append([]string{"prompt", "[approve]", "hello", "-runtime", "fake", "-approve", "allow"}, mode...)...)
		if code != 0 || strings.TrimSpace(out) != "Echo: hello" {
			t.Fatalf("prompt %v: code %d out %q err %q", mode, code, out, errOut)
		}
		if !strings.Contains(errOut, "auto-approved") || !strings.Contains(errOut, "resume with: harness-kit prompt -resume ses_") {
			t.Fatalf("prompt %v stderr: %q", mode, errOut)
		}
	}
	// Without a terminal, approvals default to deny.
	_, errOut, code := run(t, "", "prompt", "-runtime", "fake", "[approve] x")
	if code != 0 || !strings.Contains(errOut, "declined") {
		t.Fatalf("default deny: %d %q", code, errOut)
	}
	// Failed turns exit 1; unsafe options are refused.
	if _, _, code := run(t, "", "prompt", "-runtime", "fake", "[fail]"); code != 1 {
		t.Fatalf("fail exit code %d", code)
	}
	if _, errOut, code := run(t, "", "prompt", "-runtime", "fake", "-options", `{"unsafe":true}`, "hi"); code != 1 || !strings.Contains(errOut, "safety") {
		t.Fatalf("unsafe: %d %q", code, errOut)
	}
	if _, _, code := run(t, "", "prompt", "-runtime", "fake", "-allow-unsafe", "-options", `{"unsafe":true}`, "hi"); code != 0 {
		t.Fatalf("allow-unsafe: %d", code)
	}
	if _, errOut, code := run(t, "", "prompt", "-runtime", "fake", "-options", `{not json`, "hi"); code != 1 || !strings.Contains(errOut, "JSON") {
		t.Fatalf("bad options: %d %q", code, errOut)
	}
}

func TestCLIOutputFormats(t *testing.T) {
	for _, mode := range [][]string{nil, {"-rpc"}} {
		// --output json: one result object, prompt read from stdin.
		out, errOut, code := run(t, "from stdin", append([]string{"prompt", "-runtime", "fake", "--json"}, mode...)...)
		var res struct {
			SessionID, Runtime, RuntimeSessionID, TurnID, Status, Text string
			Usage                                                      map[string]any
		}
		if code != 0 || json.Unmarshal([]byte(out), &res) != nil {
			t.Fatalf("json %v: %d %q %q", mode, code, out, errOut)
		}
		if res.Status != "completed" || res.Text != "Echo: from stdin" || res.Runtime != "fake" || res.RuntimeSessionID == "" || res.Usage == nil {
			t.Fatalf("json result %v: %+v", mode, res)
		}
		if strings.Contains(errOut, "resume with") {
			t.Fatalf("json mode must keep stderr quiet: %q", errOut)
		}

		// --output stream-json: every event, starting with session.started.
		out, _, code = run(t, "", append([]string{"prompt", "-runtime", "fake", "--output", "stream-json", "hi"}, mode...)...)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		var first, last map[string]any
		json.Unmarshal([]byte(lines[0]), &first)
		json.Unmarshal([]byte(lines[len(lines)-1]), &last)
		if code != 0 || first["type"] != "session.started" || last["type"] != "turn.completed" {
			t.Fatalf("stream-json %v: %v ... %v", mode, first, last)
		}
	}
	// A failed turn still prints a result object, and exits 1.
	out, _, code := run(t, "", "prompt", "-runtime", "fake", "-o", "json", "[fail]")
	if code != 1 || !strings.Contains(out, `"status": "failed"`) {
		t.Fatalf("failed json: %d %q", code, out)
	}
	// chat --json prints one line per turn.
	out, _, code = run(t, "a\nb\n", "chat", "-runtime", "fake", "--json")
	if n := len(strings.Split(strings.TrimSpace(out), "\n")); code != 0 || n != 2 {
		t.Fatalf("chat json: %d lines=%d %q", code, n, out)
	}
	if _, errOut, code := run(t, "", "prompt", "-runtime", "fake", "--output", "yaml", "x"); code != 1 || !strings.Contains(errOut, "--output must be") {
		t.Fatalf("bad format: %d %q", code, errOut)
	}
	if _, errOut, code := run(t, "", "runtimes", "--output", "stream-json"); code != 1 || !strings.Contains(errOut, "--output must be") {
		t.Fatalf("runtimes stream-json: %d %q", code, errOut)
	}
}

func TestCLIChatFromPipe(t *testing.T) {
	out, errOut, code := run(t, "one\ntwo\n/quit\n", "chat", "-runtime", "fake")
	if code != 0 || !strings.Contains(out, "Echo: one") || !strings.Contains(out, "Echo: two") {
		t.Fatalf("chat: %d out %q err %q", code, out, errOut)
	}
}

func TestCLIHelpAndVersion(t *testing.T) {
	if out, _, code := run(t, "", "help"); code != 0 || !strings.Contains(out, "harness-kit prompt") {
		t.Fatalf("help: %d %q", code, out)
	}
	if out, _, code := run(t, "", "--version"); code != 0 || !strings.HasPrefix(out, "harness-kit ") {
		t.Fatalf("version: %d %q", code, out)
	}
	if _, _, code := run(t, "", "frobnicate"); code != 2 {
		t.Fatalf("unknown command exit %d", code)
	}
}

func TestCLISessions(t *testing.T) {
	store := t.TempDir()
	sf := []string{"--store", store}
	// Empty history.
	if out, _, code := run(t, "", append([]string{"sessions", "--json"}, sf...)...); code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty: %d %q", code, out)
	}
	// Two sessions, one via the sidecar.
	run(t, "", append([]string{"prompt", "-runtime", "fake", "first session"}, sf...)...)
	out, _, _ := run(t, "", append([]string{"prompt", "-runtime", "fake", "-rpc", "--json", "second session"}, sf...)...)
	var res struct{ SessionID string }
	json.Unmarshal([]byte(out), &res)
	if !strings.HasPrefix(res.SessionID, "ses_") {
		t.Fatalf("result session id: %q", out)
	}

	var list []struct {
		SessionID, Runtime, Title, LastStatus string
		Turns                                 int
	}
	out, _, code := run(t, "", append([]string{"sessions", "list", "--json"}, sf...)...)
	if code != 0 || json.Unmarshal([]byte(out), &list) != nil || len(list) != 2 || list[0].Title != "second session" {
		t.Fatalf("list: %d %q", code, out)
	}
	if out, _, _ := run(t, "", append([]string{"sessions"}, sf...)...); !strings.Contains(out, "first session") || !strings.Contains(out, "SESSION") {
		t.Fatalf("text list: %q", out)
	}

	// Resume by Harness Kit ID: same session, both paths, no -runtime needed.
	for _, mode := range [][]string{nil, {"-rpc"}} {
		out, errOut, code := run(t, "", append(append([]string{"prompt", "-resume", res.SessionID, "--json", "again"}, sf...), mode...)...)
		var again struct{ SessionID, Text string }
		if code != 0 || json.Unmarshal([]byte(out), &again) != nil || again.SessionID != res.SessionID || again.Text != "Echo: again" {
			t.Fatalf("resume %v: %d %q %q", mode, code, out, errOut)
		}
	}
	out, _, _ = run(t, "", append([]string{"sessions", "show", res.SessionID, "--json"}, sf...)...)
	var shown struct{ Turns int }
	if json.Unmarshal([]byte(out), &shown) != nil || shown.Turns != 3 {
		t.Fatalf("show: %q", out)
	}

	// Rename: unquoted words, both paths; -title at creation and on resume.
	if _, errOut, code := run(t, "", append([]string{"sessions", "rename", res.SessionID, "Parser", "work"}, sf...)...); code != 0 || !strings.Contains(errOut, `"Parser work"`) {
		t.Fatalf("rename: %d %q", code, errOut)
	}
	var renamed struct{ SessionID, Title string }
	out, _, code = run(t, "", append([]string{"sessions", "rename", res.SessionID, "Parser fixes", "-rpc", "--json"}, sf...)...)
	if code != 0 || json.Unmarshal([]byte(out), &renamed) != nil || renamed.Title != "Parser fixes" || renamed.SessionID != res.SessionID {
		t.Fatalf("rename over rpc: %d %q", code, out)
	}
	if _, errOut, code := run(t, "", append([]string{"sessions", "rename", res.SessionID}, sf...)...); code == 0 || !strings.Contains(errOut, "usage: harness-kit sessions rename") {
		t.Fatalf("rename without a title: %d %q", code, errOut)
	}
	if _, errOut, code := run(t, "", append([]string{"sessions", "rename", res.SessionID, " "}, sf...)...); code != 1 || !strings.Contains(errOut, "title is empty") {
		t.Fatalf("rename to blank: %d %q", code, errOut)
	}
	out, _, _ = run(t, "", append([]string{"prompt", "-runtime", "fake", "-title", "Named up front", "--json", "hello"}, sf...)...)
	var titled struct{ SessionID string }
	json.Unmarshal([]byte(out), &titled)
	run(t, "", append([]string{"prompt", "-resume", res.SessionID, "-title", "Renamed on resume", "again"}, sf...)...)
	for id, want := range map[string]string{titled.SessionID: "Named up front", res.SessionID: "Renamed on resume"} {
		out, _, _ = run(t, "", append([]string{"sessions", "show", id, "--json"}, sf...)...)
		var got struct{ Title string }
		if json.Unmarshal([]byte(out), &got) != nil || got.Title != want {
			t.Fatalf("title of %s: %q, want %q", id, out, want)
		}
	}
	run(t, "", append([]string{"sessions", "delete", titled.SessionID}, sf...)...)

	// Errors.
	if _, errOut, code := run(t, "", append([]string{"prompt", "-resume", "ses_nope", "x"}, sf...)...); code != 1 || !strings.Contains(errOut, "no session ses_nope") {
		t.Fatalf("missing: %d %q", code, errOut)
	}
	if _, errOut, code := run(t, "", "prompt", "-resume", res.SessionID, "--store", "memory", "x"); code != 1 || !strings.Contains(errOut, "needs a session store") {
		t.Fatalf("no store: %d %q", code, errOut)
	}
	if _, errOut, code := run(t, "", "sessions", "--native"); code != 1 || !strings.Contains(errOut, "needs -runtime") {
		t.Fatalf("native without runtime: %d %q", code, errOut)
	}
	if _, errOut, code := run(t, "", "sessions", "--native", "-runtime", "claude", "--claude-path", "/nope"); code != 1 || !strings.Contains(errOut, "does not offer a way to list") {
		t.Fatalf("claude native: %d %q", code, errOut)
	}
	if out, _, code := run(t, "", "sessions", "--native", "-runtime", "fake", "--json"); code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("fake native: %d %q", code, out)
	}

	// Delete.
	if _, _, code := run(t, "", append([]string{"sessions", "delete", res.SessionID}, sf...)...); code != 0 {
		t.Fatalf("delete exit %d", code)
	}
	out, _, _ = run(t, "", append([]string{"sessions", "--json"}, sf...)...)
	if strings.Contains(out, res.SessionID) {
		t.Fatalf("still listed after delete: %s", out)
	}
}

// writeEchoServer writes a tiny stdio MCP server script for tests.
func echoServerCmd(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 needed for the test MCP server")
	}
	p := filepath.Join(t.TempDir(), "echo_mcp.py")
	os.WriteFile(p, []byte(`import json, sys
for line in sys.stdin:
    m = json.loads(line)
    if m.get("id") is None: continue
    r = {"initialize": {"protocolVersion": "2025-06-18", "capabilities": {"tools": {}}, "serverInfo": {"name": "echo", "version": "1"}},
         "tools/list": {"tools": [{"name": "echo", "inputSchema": {"type": "object"}}, {"name": "rm", "inputSchema": {"type": "object"}}]}}.get(m["method"])
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": m["id"], "result": r}) + "\n"); sys.stdout.flush()
`), 0o600)
	return []string{"python3", "-I", p}
}

func TestCLIMCP(t *testing.T) {
	dir := t.TempDir()
	env := func(extra ...string) []string {
		return append([]string{"HARNESS_KIT_CONFIG=" + filepath.Join(dir, "cfg", "config.json"), "HARNESS_KIT_STORE=memory"}, extra...)
	}
	runIn := func(cwd string, extraEnv []string, args ...string) (string, string, int) {
		t.Helper()
		cmd := exec.Command(binary(t), args...)
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(), extraEnv...)
		var o, e bytes.Buffer
		cmd.Stdout, cmd.Stderr = &o, &e
		err := cmd.Run()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		return o.String(), e.String(), code
	}
	repo := filepath.Join(dir, "repo")
	os.MkdirAll(repo, 0o755)
	echo := echoServerCmd(t)

	// add / test / list (user scope)
	if _, errOut, code := runIn(repo, env(), append([]string{"mcp", "add", "echo", "--env", "HK_T", "--deny", "rm", "--"}, echo...)...); code != 0 {
		t.Fatalf("add: %d %s", code, errOut)
	}
	out, _, code := runIn(repo, env("HK_T=1"), "mcp", "test", "echo")
	if code != 0 || !strings.Contains(out, "✓ echo") || !strings.Contains(out, "rm  (filtered out)") {
		t.Fatalf("test: %d %q", code, out)
	}
	if _, _, code := runIn(repo, env(), "mcp", "test", "echo"); code != 1 {
		t.Fatalf("test without HK_T should fail, got %d", code)
	}
	out, _, _ = runIn(repo, env(), "mcp", "list", "--json")
	var listed struct {
		Servers []struct {
			Name, Source string
			Loaded       bool
		}
	}
	if json.Unmarshal([]byte(out), &listed) != nil || len(listed.Servers) != 1 || listed.Servers[0].Source != "user" {
		t.Fatalf("list: %q", out)
	}

	// An untrusted project file: listed but not loaded, with a warning.
	os.WriteFile(filepath.Join(repo, "harness-kit.json"), []byte(`{"mcpServers": {"repo": {"command": "true"}}}`), 0o644)
	_, errOut, _ := runIn(repo, env("HK_T=1"), "prompt", "-runtime", "fake", "hi")
	if !strings.Contains(errOut, "[mcp] MCP servers: echo") || !strings.Contains(errOut, "untrusted") {
		t.Fatalf("untrusted project: %q", errOut)
	}
	runIn(repo, env(), "mcp", "trust")
	for _, mode := range [][]string{nil, {"-rpc"}} {
		_, errOut, _ = runIn(repo, env("HK_T=1"), append([]string{"prompt", "-runtime", "fake", "hi"}, mode...)...)
		if !strings.Contains(errOut, "[mcp] MCP servers: echo, repo") || strings.Contains(errOut, "untrusted") {
			t.Fatalf("trusted project %v: %q", mode, errOut)
		}
	}
	// --mcp adds a file for one session; --no-kit-mcp ignores user/project.
	extra := filepath.Join(dir, "extra.json")
	os.WriteFile(extra, []byte(`{"mcpServers": {"extra": {"command": "true"}}}`), 0o600)
	_, errOut, _ = runIn(repo, env("HK_T=1"), "prompt", "-runtime", "fake", "--no-kit-mcp", "--mcp", extra, "hi")
	if !strings.Contains(errOut, "[mcp] MCP servers: extra") {
		t.Fatalf("--mcp/--no-kit-mcp: %q", errOut)
	}
	// Project-scoped add writes harness-kit.json and keeps it trusted.
	if _, errOut, code := runIn(repo, env(), "mcp", "add", "web", "--project", "--url", "https://mcp.example.com/mcp", "--bearer-env", "WEB_TOKEN"); code != 0 {
		t.Fatalf("project add: %s", errOut)
	}
	out, _, _ = runIn(repo, env(), "mcp", "list")
	if !strings.Contains(out, "web") || strings.Contains(out, "untrusted") {
		t.Fatalf("project add should stay trusted: %q", out)
	}
	b, _ := os.ReadFile(filepath.Join(repo, "harness-kit.json"))
	if !strings.Contains(string(b), "Bearer ${WEB_TOKEN}") {
		t.Fatalf("project file: %s", b)
	}
	// Bad input is rejected.
	if _, _, code := runIn(repo, env(), "mcp", "add", "bad__name", "--", "x"); code != 1 {
		t.Fatal("invalid name accepted")
	}
	if _, _, code := runIn(repo, env(), "mcp", "add", "x", "--url", "https://x", "--", "cmd"); code != 1 {
		t.Fatal("command and url accepted together")
	}
	if _, errOut, code := runIn(repo, env(), "mcp", "remove", "echo"); code != 0 || !strings.Contains(errOut, "removed") {
		t.Fatalf("remove: %d %s", code, errOut)
	}
	if _, _, code := runIn(repo, env(), "mcp", "remove", "echo"); code != 1 {
		t.Fatal("removing a missing server should fail")
	}
}
