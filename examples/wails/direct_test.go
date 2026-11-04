package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/permission"
	"github.com/tomanagle/harness-kit/core/toolhost"
)

// The test binary doubles as the MCP bridge, as the app binary does.
func TestMain(m *testing.M) {
	toolhost.ServeBridgeIfRequested()
	os.Exit(m.Run())
}

// TestDirectAPI exercises the bindings the dashboard calls in direct mode.
func TestDirectAPI(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HARNESS_KIT_STORE", dir+"/sessions")
	t.Setenv("HARNESS_KIT_CONFIG", dir+"/config.json")
	d := NewDirect()
	d.ctx = context.Background()
	defer d.shutdown(context.Background())
	d.sub = d.mgr.Subscribe(nil, 0)

	ov, err := d.Overview()
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Tools) != 2 || ov.Tools[0].Name != "order_status" || !ov.MCP.Enabled || len(ov.Runtimes) != 3 {
		t.Fatalf("overview: %+v", ov)
	}
	info, err := d.CreateSession("fake")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Prompt(info.ID, `[call order_status {"id":"A7"}] [call refund {"id":"A7"}]`); err != nil {
		t.Fatal(err)
	}
	var outputs []string
	timeout := time.After(20 * time.Second)
wait:
	for {
		select {
		case ev := <-d.sub.C:
			switch data := ev.Data.(type) {
			case permission.Request:
				d.RespondApproval(data.ID, "allow")
			case event.ToolCompletedData:
				outputs = append(outputs, data.Output)
			case event.TurnCompletedData:
				break wait
			}
		case <-timeout:
			t.Fatal("turn did not finish")
		}
	}
	if len(outputs) != 2 || !strings.Contains(outputs[0], `"status":"shipped"`) || outputs[1] != "Order A7 refunded" {
		t.Fatalf("tool outputs: %q", outputs)
	}
	list, _ := d.ListSessions()
	if len(list) != 1 || list[0].Turns != 1 {
		t.Fatalf("sessions: %+v", list)
	}
	if hist, _ := d.History(info.ID); len(hist.Events) < 5 {
		t.Fatalf("history: %d events", len(hist.Events))
	}
	if r, err := d.RenameSession(info.ID, "Refund A7"); err != nil || r.Title != "Refund A7" {
		t.Fatalf("rename: %+v %v", r, err)
	}
	if r, err := d.OpenSession(info.ID); err != nil || r.Title != "Refund A7" {
		t.Fatalf("open after rename: %+v %v", r, err)
	}
	if err := d.mgr.Close(d.ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	// A fresh backend can read history without starting the runtime; the
	// next prompt resumes it under the same ID.
	d2 := NewDirect()
	d2.ctx = context.Background()
	d2.sub = d2.mgr.Subscribe(nil, 0)
	defer d2.shutdown(context.Background())
	if r, err := d2.OpenSession(info.ID); err != nil || r.State != "closed" {
		t.Fatalf("read-only open: %+v %v", r, err)
	}
	if hist, err := d2.History(info.ID); err != nil || hist.Truncated || len(hist.Events) < 5 {
		t.Fatalf("saved history: %+v %v", hist, err)
	}
	turnID, err := d2.Prompt(info.ID, "continue")
	if err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case ev := <-d2.sub.C:
			if ev.Type == event.TurnCompleted && ev.TurnID == turnID {
				return
			}
		case <-timeout:
			t.Fatal("restored turn did not finish")
		}
	}
}

func TestBridgeFilter(t *testing.T) {
	cases := []struct {
		line          string
		forward       bool
		replyContains string
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"session.prompt","params":{}}`, true, ""},
		{`{"jsonrpc":"2.0","id":2,"method":"tools.register","params":{}}`, true, ""},
		{`{"jsonrpc":"2.0","id":3,"result":{"content":[]}}`, true, ""}, // a tool.call response
		{`{"jsonrpc":"2.0","id":4,"method":"evil.exec","params":{}}`, false, "not allowed by host: evil.exec"},
		{`{"jsonrpc":"2.0","method":"evil.notify"}`, false, ""},
	}
	for _, c := range cases {
		fwd, reply, err := filter(c.line)
		if err != nil || fwd != c.forward || (c.replyContains == "") != (reply == "") || !strings.Contains(reply, c.replyContains) {
			t.Errorf("%s: forward=%v reply=%q err=%v", c.line, fwd, reply, err)
		}
	}
	if _, _, err := filter("{not json"); err == nil {
		t.Error("malformed input should fail")
	}
}
