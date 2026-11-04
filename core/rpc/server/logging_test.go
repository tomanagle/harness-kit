package server_test

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/internal/jsonrpc"
	"github.com/tomanagle/harness-kit/core/logging"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
	"github.com/tomanagle/harness-kit/core/rpc/server"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
)

func logConnection(t *testing.T, mgr *session.Manager) (*jsonrpc.Conn, <-chan logging.Notification, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	a, b := net.Pipe()
	served := make(chan struct{})
	go func() { defer close(served); _ = server.New(mgr, server.Options{}).Serve(ctx, b, b) }()
	logs := make(chan logging.Notification, 20)
	rpc := jsonrpc.NewConn(a, a, jsonrpc.HandlerFunc(func(_ context.Context, req *jsonrpc.Request) (any, error) {
		if req.Method == protocol.NotificationLog {
			var notification logging.Notification
			if err := json.Unmarshal(req.Params, &notification); err != nil {
				return nil, err
			}
			logs <- notification
		}
		return nil, nil
	}), jsonrpc.Options{CancelMethod: protocol.NotificationCancelRequest})
	go func() { _ = rpc.Run(ctx) }()
	t.Cleanup(func() { cancel(); _ = a.Close(); _ = b.Close(); <-served })
	var initialized protocol.InitializeResult
	if err := rpc.Call(ctx, protocol.MethodInitialize, protocol.InitializeParams{ProtocolVersion: protocol.Version}, &initialized); err != nil {
		t.Fatal(err)
	}
	if !initialized.Capabilities.Logs {
		t.Fatal("logs capability missing")
	}
	return rpc, logs, ctx
}
func nextLog(t *testing.T, ctx context.Context, ch <-chan logging.Notification) logging.Notification {
	t.Helper()
	select {
	case n := <-ch:
		return n
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return logging.Notification{}
	}
}

func TestLogSubscriptionsIsolationSettingsAndLoss(t *testing.T) {
	mgr := session.NewManager(agent.NewRegistry(), session.Options{})
	t.Cleanup(func() { _ = mgr.Shutdown(context.Background()) })
	a, eventsA, ctx := logConnection(t, mgr)
	b, eventsB, _ := logConnection(t, mgr)
	logger := mgr.Logger().With("sessionId", "ses_log").WithGroup("details")
	logger.Info("before subscription")
	for _, rpc := range []*jsonrpc.Conn{a, b} {
		if err := rpc.Call(ctx, protocol.MethodLogsSubscribe, protocol.Empty{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	logger.Info("shared", "value", 7)
	first, second := nextLog(t, ctx, eventsA), nextLog(t, ctx, eventsB)
	if first.Record == nil || second.Record == nil || first.Record.Message != "shared" || first.Record.Seq != second.Record.Seq {
		t.Fatalf("shared: %+v %+v", first, second)
	}
	if string(first.Record.Attrs["sessionId"]) != `"ses_log"` || string(first.Record.Attrs["details"]) != `{"value":7}` {
		t.Fatalf("attributes: %+v", first.Record.Attrs)
	}
	if err := a.Call(ctx, protocol.MethodLogsUnsubscribe, protocol.Empty{}, nil); err != nil {
		t.Fatal(err)
	}
	logger.Info("second only")
	if n := nextLog(t, ctx, eventsB); n.Record == nil || n.Record.Message != "second only" {
		t.Fatalf("unsubscribe: %+v", n)
	}
	if err := a.Call(ctx, protocol.MethodLogsSubscribe, protocol.Empty{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.Call(ctx, protocol.MethodSettingsConfigure, settings.ConfigureParams{Patch: settings.Patch{"logging": json.RawMessage(`{"level":"error"}`)}}, nil); err != nil {
		t.Fatal(err)
	}
	logger.Info("filtered")
	logger.Error("visible")
	for _, ch := range []<-chan logging.Notification{eventsA, eventsB} {
		if n := nextLog(t, ctx, ch); n.Record == nil || n.Record.Message != "visible" || n.Record.Level != "ERROR" {
			t.Fatalf("live level: %+v", n)
		}
	}
	if err := a.Call(ctx, protocol.MethodSettingsConfigure, settings.ConfigureParams{Patch: settings.Patch{"logging": json.RawMessage(`{"maxRecordBytes":1}`)}}, nil); err != nil {
		t.Fatal(err)
	}
	logger.Error("too large for configured limit")
	for _, ch := range []<-chan logging.Notification{eventsA, eventsB} {
		if n := nextLog(t, ctx, ch); n.Dropped == nil || n.Dropped.Count != 1 {
			t.Fatalf("drop marker: %+v", n)
		}
	}
}
