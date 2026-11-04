package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/internal/jsonrpc"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
	"github.com/tomanagle/harness-kit/core/rpc/server"
	"github.com/tomanagle/harness-kit/core/runtimes/fake"
	"github.com/tomanagle/harness-kit/core/session"
)

type contextRuntime struct {
	*fake.Runtime
	started chan struct{}
	finish  chan struct{}
}

func (r *contextRuntime) Info() agent.Info {
	i := r.Runtime.Info()
	i.Capabilities.Compaction = true
	i.Capabilities.ContextUsage = true
	return i
}
func (r *contextRuntime) Start(ctx context.Context, cfg agent.SessionConfig, h agent.Handler) (agent.Session, error) {
	s, err := r.Runtime.Start(ctx, cfg, h)
	return &contextSession{Session: s, runtime: r, handler: h}, err
}

type contextSession struct {
	agent.Session
	runtime *contextRuntime
	handler agent.Handler
}

func (s *contextSession) CanCompact() bool { return true }
func (s *contextSession) Compact(ctx context.Context) error {
	s.runtime.started <- struct{}{}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.runtime.finish:
		used, capacity := int64(25), int64(100)
		s.handler.Emit(event.Event{Type: event.ContextUsage, Data: event.ContextUsageData{UsedTokens: &used, WindowTokens: &capacity, Basis: "reported"}})
		return nil
	}
}

func contextRPC(t *testing.T) (*jsonrpc.Conn, *contextRuntime, <-chan event.Event, context.Context, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	runtime := &contextRuntime{Runtime: fake.New(), started: make(chan struct{}, 1), finish: make(chan struct{}, 1)}
	mgr := session.NewManager(agent.NewRegistry(runtime), session.Options{})
	srv := server.New(mgr, server.Options{})
	a, b := net.Pipe()
	served := make(chan struct{})
	go func() { defer close(served); _ = srv.Serve(ctx, b, b) }()
	events := make(chan event.Event, 100)
	rpc := jsonrpc.NewConn(a, a, jsonrpc.HandlerFunc(func(_ context.Context, req *jsonrpc.Request) (any, error) {
		if req.Method == protocol.NotificationEvent {
			var ev event.Event
			if err := json.Unmarshal(req.Params, &ev); err != nil {
				return nil, err
			}
			events <- ev
		}
		return nil, nil
	}), jsonrpc.Options{CancelMethod: protocol.NotificationCancelRequest})
	go func() { _ = rpc.Run(ctx) }()
	t.Cleanup(func() {
		shutdown, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = rpc.Call(shutdown, protocol.MethodShutdown, struct{}{}, nil)
		cancel()
		_ = a.Close()
		_ = b.Close()
		<-served
	})
	if err := rpc.Call(ctx, protocol.MethodInitialize, protocol.InitializeParams{ProtocolVersion: protocol.Version, ClientInfo: protocol.ClientInfo{Name: "context-test"}}, nil); err != nil {
		t.Fatal(err)
	}
	var created protocol.SessionResult
	if err := rpc.Call(ctx, protocol.MethodSessionCreate, protocol.SessionCreateParams{Runtime: "fake"}, &created); err != nil {
		t.Fatal(err)
	}
	return rpc, runtime, events, ctx, created.Session.ID
}

func waitContextEvent(t *testing.T, ctx context.Context, events <-chan event.Event, status string) {
	t.Helper()
	for {
		select {
		case ev := <-events:
			if ev.Type == event.ContextCompaction && ev.Data.(event.ContextCompactionData).Status == status {
				return
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestContextRPCUnknownAndNativeCompletion(t *testing.T) {
	rpc, runtime, events, ctx, id := contextRPC(t)
	p := protocol.SessionIDParams{SessionID: id}
	var raw map[string]json.RawMessage
	if err := rpc.Call(ctx, protocol.MethodSessionContext, p, &raw); err != nil {
		t.Fatal(err)
	}
	var usage map[string]any
	if err := json.Unmarshal(raw["usage"], &usage); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"usedTokens", "windowTokens"} {
		if value, ok := usage[field]; !ok || value != nil {
			t.Fatalf("%s must be explicit null: %s", field, raw["usage"])
		}
	}
	for _, method := range []string{protocol.MethodSessionContext, protocol.MethodSessionCompact} {
		var rpcErr *jsonrpc.Error
		err := rpc.Call(ctx, method, map[string]any{"sessionId": id, "surprise": true}, nil)
		if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
			t.Fatalf("strict %s: %v", method, err)
		}
	}
	result := make(chan session.ContextInfo, 1)
	errs := make(chan error, 1)
	go func() {
		var v session.ContextInfo
		err := rpc.Call(ctx, protocol.MethodSessionCompact, p, &v)
		result <- v
		errs <- err
	}()
	select {
	case <-runtime.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-errs:
		t.Fatal("compact returned before native completion")
	default:
	}
	var inFlight session.ContextInfo
	if err := rpc.Call(ctx, protocol.MethodSessionContext, p, &inFlight); err != nil {
		t.Fatal(err)
	}
	if inFlight.State != session.StateCompacting || inFlight.CanCompact {
		t.Fatalf("in flight: %+v", inFlight)
	}
	err := rpc.Call(ctx, protocol.MethodSessionPrompt, protocol.SessionPromptParams{SessionID: id, TurnID: "blocked", Input: agent.Text("blocked")}, nil)
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != protocol.CodeSessionBusy {
		t.Fatalf("prompt during compact: %v", err)
	}
	runtime.finish <- struct{}{}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	after := <-result
	if after.State != session.StateIdle || !after.CanCompact || after.Usage.UsedTokens == nil || *after.Usage.UsedTokens != 25 || after.Usage.Stale {
		t.Fatalf("after: %+v", after)
	}
	waitContextEvent(t, ctx, events, "completed")
}

func TestContextRPCCancellationRemainsAvailable(t *testing.T) {
	for _, via := range []string{"session.cancel", "session.close", "request"} {
		t.Run(via, func(t *testing.T) {
			rpc, runtime, events, ctx, id := contextRPC(t)
			request, cancel := context.WithCancel(ctx)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- rpc.Call(request, protocol.MethodSessionCompact, protocol.SessionIDParams{SessionID: id}, nil)
			}()
			select {
			case <-runtime.started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if via == "request" {
				cancel()
			} else if err := rpc.Call(ctx, via, protocol.SessionIDParams{SessionID: id}, nil); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("cancelled compaction succeeded")
				}
			case <-ctx.Done():
				t.Fatal("cancellation blocked behind compaction")
			}
			waitContextEvent(t, ctx, events, "cancelled")
		})
	}
}
