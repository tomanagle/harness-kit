package harnesskit_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	hk "github.com/tomanagle/harness-kit/packages/client-go"
	"github.com/tomanagle/harness-kit/packages/client-go/internal/jsonrpc"
)

func server(t *testing.T, handler jsonrpc.HandlerFunc) (net.Conn, *jsonrpc.Conn, <-chan struct{}) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	rpc := jsonrpc.NewConn(serverConn, serverConn, jsonrpc.HandlerFunc(func(ctx context.Context, req *jsonrpc.Request) (any, error) {
		if req.Method == hk.MethodInitialize {
			return hk.InitializeResult{ProtocolVersion: hk.Version, ServerInfo: hk.ServerInfo{Name: "fixture", Version: "test"}}, nil
		}
		return handler(ctx, req)
	}), jsonrpc.Options{CancelMethod: hk.NotificationCancelRequest})
	done := make(chan struct{})
	go func() { defer close(done); _ = rpc.Run(context.Background()) }()
	t.Cleanup(func() { _ = clientConn.Close(); _ = serverConn.Close() })
	return clientConn, rpc, done
}

func timeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for peer")
		var zero T
		return zero
	}
}

func TestGeneratedMethodsAndStructuredErrors(t *testing.T) {
	conn, _, _ := server(t, func(_ context.Context, req *jsonrpc.Request) (any, error) {
		switch req.Method {
		case hk.MethodSessionCreate:
			var p hk.CreateSessionParams
			if err := req.UnmarshalParams(&p); err != nil {
				return nil, err
			}
			if p.Runtime != "fake" {
				t.Errorf("runtime = %q", p.Runtime)
			}
			return map[string]any{"session": map[string]any{"sessionId": "ses_one", "runtime": p.Runtime, "state": "idle"}}, nil
		case hk.MethodSessionGet:
			return nil, jsonrpc.NewError(hk.CodeSessionNotFound, "missing", map[string]any{"reason": hk.ReasonSessionNotFound, "future": map[string]bool{"preserved": true}})
		default:
			return nil, jsonrpc.NewError(jsonrpc.CodeMethodNotFound, req.Method, nil)
		}
	})
	c, err := hk.Connect(timeout(t), conn, conn, hk.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if c.Info().ServerInfo.Name != "fixture" {
		t.Fatalf("initialize: %+v", c.Info())
	}
	created, err := c.SessionCreate(timeout(t), hk.CreateSessionParams{Runtime: "fake"})
	if err != nil || created.Session.ID != "ses_one" {
		t.Fatalf("create: %+v, %v", created, err)
	}
	_, err = c.SessionGet(timeout(t), hk.SessionIDParams{SessionID: "missing"})
	var rpcErr *hk.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != hk.CodeSessionNotFound || rpcErr.Reason != hk.ReasonSessionNotFound || !strings.Contains(string(rpcErr.Data), `"preserved":true`) {
		t.Fatalf("structured error: %#v", err)
	}
}

func TestNotificationsAndReverseToolCancellation(t *testing.T) {
	conn, rpc, _ := server(t, func(_ context.Context, req *jsonrpc.Request) (any, error) {
		if req.Method == hk.MethodToolsRegister {
			return map[string]any{}, nil
		}
		if req.Method == hk.MethodToolsUnregister {
			return nil, jsonrpc.NewError(jsonrpc.CodeInternalError, "retry later", nil)
		}
		return nil, jsonrpc.NewError(jsonrpc.CodeMethodNotFound, req.Method, nil)
	})
	events := make(chan hk.HarnessEvent, 1)
	snapshots := make(chan hk.SettingsSnapshot, 1)
	c, err := hk.Connect(timeout(t), conn, conn, hk.Options{OnEvent: func(ev hk.HarnessEvent) { events <- ev }, OnSettings: func(s hk.SettingsSnapshot) { snapshots <- s }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := rpc.Notify(timeout(t), hk.NotificationEvent, map[string]any{"seq": 1, "type": "future.event", "sessionId": "s", "time": "2026-01-01T00:00:00Z", "data": map[string]string{"new": "retained"}}); err != nil {
		t.Fatal(err)
	}
	ev := receive(t, events)
	if string(ev.Type) != "future.event" || string(ev.Data) != `{"new":"retained"}` {
		t.Fatalf("unknown event: %+v", ev)
	}
	if err := rpc.Notify(timeout(t), hk.NotificationSettingsUpdated, map[string]any{"revision": 9, "resync": true}); err != nil {
		t.Fatal(err)
	}
	if snapshot := receive(t, snapshots); snapshot.Revision != 9 || !snapshot.Resync {
		t.Fatalf("snapshot: %+v", snapshot)
	}
	started, aborted := make(chan struct{}, 1), make(chan struct{}, 1)
	if err := c.RegisterTool(timeout(t), hk.ToolSpec{Name: "wait"}, func(ctx context.Context, p hk.ToolCallParams) (hk.ToolResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		aborted <- struct{}{}
		return hk.ToolResult{}, ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	// A failed unregister must preserve the callback on a still-live host.
	if err := c.UnregisterTools(timeout(t), "wait"); err == nil {
		t.Fatal("expected unregister failure")
	}
	ctx, cancel := context.WithCancel(timeout(t))
	defer cancel()
	callDone := make(chan error, 1)
	go func() {
		var result hk.ToolResult
		callDone <- rpc.Call(ctx, hk.RequestToolCall, hk.ToolCallParams{Name: "wait", SessionID: "s", Arguments: json.RawMessage(`{}`)}, &result)
	}()
	receive(t, started)
	cancel()
	if err := receive(t, callDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("call cancellation: %v", err)
	}
	receive(t, aborted)
}

func TestCloseAttachedFailsPendingWithoutShutdown(t *testing.T) {
	started := make(chan struct{}, 1)
	var shutdowns atomic.Int32
	conn, _, serverDone := server(t, func(ctx context.Context, req *jsonrpc.Request) (any, error) {
		if req.Method == hk.MethodShutdown {
			shutdowns.Add(1)
			return map[string]any{}, nil
		}
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	c, err := hk.Connect(timeout(t), conn, conn, hk.Options{})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- c.Call(timeout(t), "pending", nil, nil) }()
	receive(t, started)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := receive(t, result); !errors.Is(err, hk.ErrClosed) {
		t.Fatalf("pending call: %v", err)
	}
	receive(t, c.Done())
	receive(t, serverDone)
	if shutdowns.Load() != 0 {
		t.Fatal("attached client shut down shared host")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestFailedInitialSettingsClosesTransport(t *testing.T) {
	conn, _, done := server(t, func(_ context.Context, req *jsonrpc.Request) (any, error) {
		return nil, jsonrpc.NewError(hk.CodeRestartRequired, "restart", map[string]string{"reason": hk.ReasonRestartRequired})
	})
	var closes atomic.Int32
	_, err := hk.Connect(timeout(t), conn, conn, hk.Options{
		InitialSettings: hk.SettingsPatch{"process": json.RawMessage(`{"store":"new"}`)},
		CloseTransport:  func() error { closes.Add(1); return conn.Close() },
	})
	var rpcErr *hk.Error
	if !errors.As(err, &rpcErr) || rpcErr.Reason != hk.ReasonRestartRequired {
		t.Fatalf("initial settings: %v", err)
	}
	receive(t, done)
	if closes.Load() != 1 {
		t.Fatalf("transport close count = %d", closes.Load())
	}
}

func TestUnsupportedProtocolClosesTransport(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close(); _ = serverConn.Close() })
	rpc := jsonrpc.NewConn(serverConn, serverConn, jsonrpc.HandlerFunc(func(context.Context, *jsonrpc.Request) (any, error) {
		return hk.InitializeResult{ProtocolVersion: hk.Version + 1}, nil
	}), jsonrpc.Options{})
	done := make(chan struct{})
	go func() { defer close(done); _ = rpc.Run(context.Background()) }()
	_, err := hk.Connect(timeout(t), clientConn, clientConn, hk.Options{})
	var rpcErr *hk.Error
	if !errors.As(err, &rpcErr) || rpcErr.Reason != hk.ReasonUnsupportedVersion {
		t.Fatalf("unsupported protocol: %v", err)
	}
	receive(t, done)
}

func TestRequestContextCancellation(t *testing.T) {
	started, cancelled := make(chan struct{}, 1), make(chan struct{}, 1)
	conn, _, _ := server(t, func(ctx context.Context, req *jsonrpc.Request) (any, error) {
		started <- struct{}{}
		<-ctx.Done()
		cancelled <- struct{}{}
		return nil, ctx.Err()
	})
	c, err := hk.Connect(timeout(t), conn, conn, hk.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithCancel(timeout(t))
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- c.Call(ctx, "slow", nil, nil) }()
	receive(t, started)
	cancel()
	if err := receive(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request: %v", err)
	}
	receive(t, cancelled)
}

func TestFakeSidecarIntegration(t *testing.T) {
	core := filepath.Join("..", "..", "core")
	if _, err := os.Stat(filepath.Join(core, "go.mod")); err != nil {
		t.Skip("integration fixture requires the repository checkout")
	}
	binary := filepath.Join(t.TempDir(), "harness-kit")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "-C", core, "build", "-o", binary, "./cmd/harness-kit")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build sidecar: %v\n%s", err, output)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	events := make(chan hk.HarnessEvent, 64)
	c, err := hk.Spawn(ctx, binary, []string{"serve", "--fake-runtime", "--log-level", "warn"}, io.Discard, hk.Options{
		Settings: hk.SettingsPatch{"memory": json.RawMessage(`{"eventLogSize":16}`)},
		OnEvent:  func(ev hk.HarnessEvent) { events <- ev },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	created, err := c.SessionCreate(ctx, hk.CreateSessionParams{Runtime: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.SessionPrompt(ctx, hk.SessionPromptParams{SessionID: created.Session.ID, TurnID: "go-client-smoke", Input: []hk.InputPart{{Type: "text", Text: "hello from Go"}}})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for {
		ev := receive(t, events)
		if ev.TurnID != "go-client-smoke" {
			continue
		}
		if string(ev.Type) == "message.delta" {
			var data hk.MessageDeltaData
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatal(err)
			}
			text += data.Text
		}
		if string(ev.Type) == "turn.completed" {
			break
		}
	}
	if !strings.Contains(text, "hello from Go") {
		t.Fatalf("response text = %q", text)
	}
	page, err := c.SessionEvents(ctx, hk.SessionEventsParams{SessionID: created.Session.ID})
	if err != nil || len(page.Events) == 0 {
		t.Fatalf("history: %+v, %v", page, err)
	}
	if err := c.CloseContext(ctx); err != nil {
		t.Fatalf("sidecar close: %v", err)
	}
}

func TestSpawnTerminatesUnresponsiveOwnedProcess(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := hk.Spawn(timeout(t), executable,
		[]string{"-test.run=^TestSidecarProcessFixture$", "--", "harnesskit-process-fixture"}, io.Discard, hk.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = c.CloseContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown timeout: %v", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("owned process did not stop after grace period")
	}
	receive(t, c.Done())
}

// This test is also a cross-platform subprocess fixture. It negotiates the
// protocol, then ignores shutdown and EOF so the client's kill path is exercised.
func TestSidecarProcessFixture(t *testing.T) {
	if os.Args[len(os.Args)-1] != "harnesskit-process-fixture" {
		return
	}
	rpc := jsonrpc.NewConn(os.Stdin, os.Stdout, jsonrpc.HandlerFunc(func(ctx context.Context, req *jsonrpc.Request) (any, error) {
		if req.Method == hk.MethodInitialize {
			return hk.InitializeResult{ProtocolVersion: hk.Version}, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}), jsonrpc.Options{})
	go rpc.Run(context.Background())
	for {
		time.Sleep(time.Hour)
	}
}
