package toolhost

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/mcp"
	mcpclient "github.com/tomanagle/harness-kit/mcp/client"
	"github.com/tomanagle/harness-kit/permission"
	"github.com/tomanagle/harness-kit/session"
	"github.com/tomanagle/harness-kit/tools"
)

// The test binary doubles as the bridge executable.
func TestMain(m *testing.M) {
	ServeBridgeIfRequested()
	os.Exit(m.Run())
}

type approver struct {
	mu       sync.Mutex
	requests []permission.Request
	decide   permission.Outcome
	message  string
}

func (a *approver) ask(_ context.Context, r permission.Request) (permission.Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, r)
	return permission.Decision{Outcome: a.decide, Message: a.message}, nil
}

func setup(t *testing.T, prompt bool, names []string) (*Host, *approver, *atomic.Int32, session.Attachment) {
	t.Helper()
	reg := tools.NewRegistry()
	tools.Add(reg, "greet", "Say hello", func(ctx context.Context, in struct {
		Name string `json:"name"`
	}) (string, error) {
		call, _ := tools.CallFromContext(ctx)
		return "hello " + in.Name + " from " + call.SessionID + "/" + call.TurnID, nil
	}, tools.ReadOnly())
	tools.Add(reg, "deploy", "Deploy the app", func(context.Context, struct{}) (string, error) { return "deployed", nil },
		tools.WithApproval(mcp.ApprovalAsk))
	exe, _ := os.Executable()
	h := New(Options{Registry: reg, BridgeCommand: []string{exe}})
	t.Cleanup(func() { h.Close() })
	a := &approver{decide: permission.Allow}
	var cancelled atomic.Int32
	att, err := h.Attach(context.Background(), session.ToolBinding{
		SessionID: "ses_1", Tools: names, ApprovalPrompt: prompt, Approver: a.ask,
		CurrentTurn: func() string { return "turn_1" }, CancelTurn: func() { cancelled.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	att.Server.Env["GORACE"] = "atexit_sleep_ms=0"
	return h, a, &cancelled, att
}

func connect(t *testing.T, s mcp.Server) *mcpclient.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sess, err := mcpclient.Connect(ctx, s, mcpclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.Close)
	return sess
}

func TestToolsThroughBridge(t *testing.T) {
	_, a, cancelled, att := setup(t, false, nil)
	if att.Name != ServerName || att.PermissionTool != "" || att.Server.Approval != mcp.ApprovalAuto {
		t.Fatalf("attachment: %+v", att)
	}
	sess := connect(t, att.Server)
	ctx := context.Background()
	list, err := sess.ListTools(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "greet" {
		t.Fatalf("tools: %+v %v", list, err)
	}
	res, err := sess.CallTool(ctx, "greet", json.RawMessage(`{"name":"Ada"}`))
	if err != nil || res.IsError || res.Text() != "hello Ada from ses_1/turn_1" {
		t.Fatalf("greet: %+v %v", res, err)
	}
	if len(a.requests) != 0 {
		t.Fatal("auto tools must not ask for approval")
	}
	// Invalid arguments come back as a readable error result.
	if res, _ := sess.CallTool(ctx, "greet", json.RawMessage(`{"name":7}`)); !res.IsError || !strings.Contains(res.Text(), "expected string") {
		t.Fatalf("invalid args: %+v", res)
	}

	// approval: ask goes through the host's approver.
	res, _ = sess.CallTool(ctx, "deploy", nil)
	if res.IsError || res.Text() != "deployed" || len(a.requests) != 1 || a.requests[0].Kind != permission.KindTool || a.requests[0].Tool != "deploy" {
		t.Fatalf("approved deploy: %+v %+v", res, a.requests)
	}
	a.decide, a.message = permission.Deny, "not on Fridays"
	if res, _ := sess.CallTool(ctx, "deploy", nil); !res.IsError || !strings.Contains(res.Text(), "not on Fridays") {
		t.Fatalf("denied deploy: %+v", res)
	}
	a.decide = permission.Abort
	if res, _ := sess.CallTool(ctx, "deploy", nil); !res.IsError || cancelled.Load() != 1 {
		t.Fatalf("aborted deploy: %+v cancelled=%d", res, cancelled.Load())
	}
	if res, _ := sess.CallTool(ctx, "nope", nil); !res.IsError {
		t.Fatal("unknown tool should be an error result")
	}
	if res, _ := sess.CallTool(ctx, PermissionTool, nil); !res.IsError {
		t.Fatal("the permission tool is only served when requested")
	}
}

func TestToolFilterAndNothingToServe(t *testing.T) {
	_, _, _, att := setup(t, false, []string{"greet"})
	sess := connect(t, att.Server)
	list, _ := sess.ListTools(context.Background())
	if len(list) != 1 || list[0].Name != "greet" {
		t.Fatalf("filtered: %+v", list)
	}
	if res, _ := sess.CallTool(context.Background(), "deploy", nil); !res.IsError {
		t.Fatal("filtered-out tool must not be callable")
	}

	h := New(Options{Registry: tools.NewRegistry()})
	defer h.Close()
	att2, err := h.Attach(context.Background(), session.ToolBinding{SessionID: "s"})
	if err != nil || att2.Name != "" {
		t.Fatalf("an empty registry without prompts needs no bridge: %+v %v", att2, err)
	}
}

func TestPermissionPrompt(t *testing.T) {
	_, a, cancelled, att := setup(t, true, nil)
	if att.PermissionTool != PermissionTool {
		t.Fatalf("attachment: %+v", att)
	}
	sess := connect(t, att.Server)
	ctx := context.Background()
	call := func(args string) map[string]any {
		t.Helper()
		res, err := sess.CallTool(ctx, PermissionTool, json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(res.Text()), &out); err != nil {
			t.Fatalf("not JSON: %q", res.Text())
		}
		return out
	}
	out := call(`{"tool_name":"Bash","input":{"command":"touch x","description":"make x"},"tool_use_id":"toolu_1"}`)
	if out["behavior"] != "allow" || out["updatedInput"].(map[string]any)["command"] != "touch x" {
		t.Fatalf("allow: %+v", out)
	}
	r := a.requests[0]
	if r.Kind != permission.KindCommand || r.Command != "touch x" || r.Title != "Run `touch x`" || r.Reason != "make x" {
		t.Fatalf("classified: %+v", r)
	}
	a.decide, a.message = permission.Deny, ""
	out = call(`{"tool_name":"Write","input":{"file_path":"/tmp/a.go","content":"x"}}`)
	if out["behavior"] != "deny" || out["message"] == "" || a.requests[1].Kind != permission.KindFileChange || a.requests[1].Paths[0] != "/tmp/a.go" {
		t.Fatalf("deny: %+v %+v", out, a.requests[1])
	}
	a.decide = permission.Abort
	if out := call(`{"tool_name":"WebFetch","input":{"url":"https://x"}}`); out["behavior"] != "deny" || cancelled.Load() != 1 || a.requests[2].Kind != permission.KindNetwork {
		t.Fatalf("abort: %+v", out)
	}
}

func TestBridgeSecurity(t *testing.T) {
	h, _, _, att := setup(t, false, nil)
	sock := att.Server.Env[envSocket]
	if fi, err := os.Stat(filepath.Dir(sock)); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700) {
		t.Fatalf("socket directory must be private: %v %v", fi.Mode().Perm(), err)
	}
	// A wrong token is disconnected without an answer.
	nc, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	nc.Write([]byte(`{"token":"wrong"}` + "\n" + `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"))
	nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if line, err := bufio.NewReader(nc).ReadString('\n'); err == nil {
		t.Fatalf("got a reply with a bad token: %q", line)
	}
	nc.Close()

	// Detaching revokes the token and drops live connections.
	sess := connect(t, att.Server)
	att.Detach()
	if _, err := sess.ListTools(context.Background()); err == nil {
		t.Fatal("detached session should lose access")
	}
	if _, err := mcpclient.Connect(context.Background(), att.Server, mcpclient.Options{}); err == nil {
		t.Fatal("a revoked token must not connect")
	}
	h.Close()
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatal("Close should remove the socket")
	}
}
