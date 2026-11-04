package toolhost

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tomanagle/harness-kit/core/mcp"
	"github.com/tomanagle/harness-kit/core/permission"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/tools"
)

// Implements only the consumed catalog/execution contract, without embedding
// tools.Registry or implementing its registration/change-listener methods.
type customToolRegistry struct {
	catalog []tools.Tool
	calls   []tools.Call
}

func (r *customToolRegistry) List() []tools.Tool { return r.catalog }
func (r *customToolRegistry) Get(name string) (tools.Tool, bool) {
	for _, tool := range r.catalog {
		if tool.Name == name {
			return tool, true
		}
	}
	return tools.Tool{}, false
}
func (r *customToolRegistry) Call(_ context.Context, call tools.Call) (tools.Result, error) {
	r.calls = append(r.calls, call)
	return tools.TextResult("custom backend result"), nil
}

func TestCustomToolRegistryPreservesAllowlistAndApprovals(t *testing.T) {
	registry := &customToolRegistry{catalog: []tools.Tool{
		{Name: "hidden", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "publish", InputSchema: json.RawMessage(`{"type":"object"}`), Approval: mcp.ApprovalAsk},
	}}
	host := New(Options{Registry: registry})
	t.Cleanup(func() { _ = host.Close() })
	visible := host.toolsFor([]string{"publish"})
	if len(visible) != 1 || visible[0].Name != "publish" {
		t.Fatalf("allowlist: %+v", visible)
	}
	if registry.catalog[0].Name != "hidden" || registry.catalog[1].Name != "publish" {
		t.Fatal("host filtering mutated the provider's catalog")
	}
	a := &approver{decide: permission.Deny}
	conn := &serverConn{h: host, b: &binding{ToolBinding: session.ToolBinding{
		SessionID: "session", Tools: []string{"publish"}, CurrentTurn: func() string { return "turn" }, Approver: a.ask,
	}}}
	for _, name := range []string{"hidden", "publish"} {
		if result := conn.callTool(context.Background(), name, json.RawMessage(`{}`)); !result.IsError {
			t.Fatalf("unapproved tool ran: %s", name)
		}
	}
	if len(registry.calls) != 0 || len(a.requests) != 1 {
		t.Fatalf("allowlist/approval bypass: calls=%v approvals=%v", registry.calls, a.requests)
	}
	a.decide = permission.Allow
	result := conn.callTool(context.Background(), "publish", json.RawMessage(`{"value":1}`))
	if result.IsError || result.Text() != "custom backend result" || len(registry.calls) != 1 {
		t.Fatalf("custom execution: %+v calls=%v", result, registry.calls)
	}
	call := registry.calls[0]
	if call.SessionID != "session" || call.TurnID != "turn" || call.Name != "publish" || string(call.Arguments) != `{"value":1}` {
		t.Fatalf("lost caller context: %+v", call)
	}
}

type observableToolRegistry struct {
	customToolRegistry
	callback     func()
	unsubscribed bool
}

func (r *observableToolRegistry) OnChange(callback func()) func() {
	r.callback = callback
	return func() { r.unsubscribed = true }
}

func TestCustomToolRegistryOptionalChangeNotifications(t *testing.T) {
	registry := &observableToolRegistry{}
	host := New(Options{Registry: registry})
	if registry.callback == nil {
		t.Fatal("optional change notifications were not connected")
	}
	registry.callback()
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	if !registry.unsubscribed {
		t.Fatal("change subscription was retained after host close")
	}
}
