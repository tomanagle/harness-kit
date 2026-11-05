package server_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/hooks"
	"github.com/tomanagle/harness-kit/core/internal/jsonrpc"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
	"github.com/tomanagle/harness-kit/core/rpc/server"
	"github.com/tomanagle/harness-kit/core/runtimes/fake"
	"github.com/tomanagle/harness-kit/core/session"
)

func TestHookConnectionOwnershipAndDisconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mgr := session.NewManager(agent.NewRegistry(fake.New()), session.Options{})
	defer mgr.Shutdown(ctx)
	srv := server.New(mgr, server.Options{})
	connect := func() (*jsonrpc.Conn, func()) {
		cr, sw := io.Pipe()
		sr, cw := io.Pipe()
		done := make(chan error, 1)
		go func() { done <- srv.Serve(ctx, sr, sw) }()
		c := jsonrpc.NewConn(cr, cw, jsonrpc.HandlerFunc(func(context.Context, *jsonrpc.Request) (any, error) { return nil, nil }), jsonrpc.Options{})
		go c.Run(ctx)
		if err := c.Call(ctx, protocol.MethodInitialize, protocol.InitializeParams{ProtocolVersion: protocol.Version}, new(protocol.InitializeResult)); err != nil {
			t.Fatal(err)
		}
		return c, func() {
			_ = cw.Close()
			_ = cr.Close()
			if err := <-done; err != nil {
				t.Error(err)
			}
			_ = sw.Close()
			_ = sr.Close()
		}
	}
	owner, closeOwner := connect()
	other, closeOther := connect()
	defer closeOther()
	params := protocol.HooksRegisterParams{Hook: hooks.Spec{ID: "required", Name: hooks.BeforeTurn}}
	if err := owner.Call(ctx, protocol.MethodHooksRegister, params, nil); err != nil {
		t.Fatal(err)
	}
	if err := other.Call(ctx, protocol.MethodHooksUnregister, protocol.HooksUnregisterParams{ID: "required"}, nil); err == nil {
		t.Fatal("foreign registration removed")
	}
	if err := other.Call(ctx, protocol.MethodHooksRegister, params, nil); err == nil {
		t.Fatal("duplicate ID replaced another owner")
	}
	closeOwner()
	if len(mgr.Hooks().List()) != 0 {
		t.Fatal("disconnect did not remove registrations")
	}
	if _, _, err := mgr.Hooks().Run(ctx, hooks.Request{Name: hooks.BeforeTurn}, hooks.Settings{Enabled: true, TimeoutMs: 100, MaxConcurrent: 1, OnError: "fail", Required: []string{"required"}}); err == nil {
		t.Fatal("missing required owner allowed a prompt")
	}
}
