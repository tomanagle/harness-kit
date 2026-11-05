package harnesskit_test

import (
	"context"
	"encoding/json"
	hk "github.com/tomanagle/harness-kit/packages/client-go"
	"github.com/tomanagle/harness-kit/packages/client-go/internal/jsonrpc"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRunReceivesCompletionBeforePromptResponse(t *testing.T) {
	var rpc *jsonrpc.Conn
	conn, peer, _ := server(t, func(ctx context.Context, request *jsonrpc.Request) (any, error) {
		if request.Method == hk.MethodSessionPrompt {
			var p hk.SessionPromptParams
			if err := request.UnmarshalParams(&p); err != nil {
				return nil, err
			}
			if err := rpc.Notify(ctx, hk.NotificationEvent, hk.HarnessEvent{Seq: 1, SessionID: p.SessionID, TurnID: p.TurnID, Type: "turn.completed", Data: json.RawMessage(`{"status":"completed"}`)}); err != nil {
				return nil, err
			}
			return hk.SessionPromptResult{TurnID: p.TurnID}, nil
		}
		return hk.Empty{}, nil
	})
	rpc = peer
	client, err := hk.Connect(timeout(t), conn, conn, hk.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	result, err := client.Run(timeout(t), hk.SessionPromptParams{SessionID: "s", Input: []hk.InputPart{{Type: "text", Text: "hi"}}})
	if err != nil || result.Status != "completed" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Run(ctx, hk.SessionPromptParams{SessionID: "s"}); err == nil {
		t.Fatal("aborted request accepted")
	}
}
func TestConversationProjectionAndAttachments(t *testing.T) {
	state, err := hk.NewConversationState("s", hk.ConversationOptions{MaxEvents: 2})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 10; i++ {
		raw, _ := json.Marshal(map[string]string{"itemId": "m", "text": "part"})
		state.Apply(hk.HarnessEvent{SessionID: "s", TurnID: "t", Seq: uint64(i), Type: "message.delta", Data: raw})
	}
	snapshot := state.Snapshot()
	if !snapshot.Truncated || snapshot.Messages[0].Text != "partpart" {
		t.Fatalf("snapshot: %+v", snapshot)
	}
	file := filepath.Join(t.TempDir(), "example.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	prepared, err := hk.PrepareAttachments(timeout(t), "Explain", []string{file, file}, hk.AttachmentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Input) != 2 || len(prepared.Attachments) != 1 || prepared.Input[0].Text != "Explain" {
		t.Fatalf("attachments: %+v", prepared)
	}
	limit := int64(1)
	if _, err := hk.PrepareAttachments(timeout(t), "Explain", []string{file}, hk.AttachmentOptions{MaxTextBytes: &limit}); err == nil {
		t.Fatal("attachment byte limit ignored")
	}
	options, err := hk.EncodeOptions(hk.CodexOptions{Sandbox: "workspace-write"})
	if err != nil || string(options["sandbox"]) != `"workspace-write"` {
		t.Fatalf("typed options: %v %v", options, err)
	}
}

func TestConversationObserverReplaysGapsAndCloses(t *testing.T) {
	var mu sync.Mutex
	records := []hk.HarnessEvent{{SessionID: "s", TurnID: "t", Seq: 1, Type: "message.delta", Data: json.RawMessage(`{"itemId":"m","text":"Hel"}`)}}
	conn, peer, _ := server(t, func(ctx context.Context, request *jsonrpc.Request) (any, error) {
		if request.Method == hk.MethodSessionEventsPage {
			var params hk.SessionEventsParams
			if err := request.UnmarshalParams(&params); err != nil {
				return nil, err
			}
			mu.Lock()
			defer mu.Unlock()
			page := hk.EventPage{NextSeq: params.AfterSeq}
			for _, event := range records {
				if event.Seq > params.AfterSeq {
					page.Events = append(page.Events, event)
					page.NextSeq = event.Seq
				}
			}
			return page, nil
		}
		return hk.Empty{}, nil
	})
	client, err := hk.Connect(timeout(t), conn, conn, hk.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	observer, err := client.ObserveConversation(timeout(t), "s", hk.ConversationOptions{MaxEvents: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	if got := observer.Snapshot().Messages[0].Text; got != "Hel" {
		t.Fatal(got)
	}
	mu.Lock()
	records = append(records, hk.HarnessEvent{SessionID: "s", TurnID: "t", Seq: 2, Type: "message.completed", Data: json.RawMessage(`{"itemId":"m","text":"Hello"}`)})
	mu.Unlock()
	if err := peer.Notify(timeout(t), hk.NotificationEvent, hk.HarnessEvent{SessionID: "s", Type: "stream.gap", Data: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case snapshot := <-observer.Updates:
			if snapshot.Seq == 2 {
				if snapshot.Messages[0].Text != "Hello" {
					t.Fatal(snapshot)
				}
				observer.Close()
				for range observer.Updates {
				}
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("observer failed to recover gap")
		}
	}
}

func TestRunRecoversCompletionAfterGap(t *testing.T) {
	var peer *jsonrpc.Conn
	var turnID string
	conn, rpc, _ := server(t, func(ctx context.Context, request *jsonrpc.Request) (any, error) {
		switch request.Method {
		case hk.MethodSessionPrompt:
			var params hk.SessionPromptParams
			if err := request.UnmarshalParams(&params); err != nil {
				return nil, err
			}
			turnID = params.TurnID
			if err := peer.Notify(ctx, hk.NotificationEvent, hk.HarnessEvent{SessionID: "s", Type: "stream.gap", Data: json.RawMessage(`{}`)}); err != nil {
				return nil, err
			}
			return hk.SessionPromptResult{TurnID: turnID}, nil
		case hk.MethodSessionEventsPage:
			return hk.EventPage{NextSeq: 2, Events: []hk.HarnessEvent{{SessionID: "s", TurnID: turnID, Seq: 2, Type: "turn.completed", Data: json.RawMessage(`{"status":"completed"}`)}}}, nil
		}
		return hk.Empty{}, nil
	})
	peer = rpc
	client, err := hk.Connect(timeout(t), conn, conn, hk.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	result, err := client.Run(timeout(t), hk.SessionPromptParams{SessionID: "s", Input: []hk.InputPart{{Type: "text", Text: "hi"}}})
	if err != nil || result.Status != "completed" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestRegisteredHookHandlesReverseCallsAndUnregisters(t *testing.T) {
	conn, peer, _ := server(t, func(ctx context.Context, request *jsonrpc.Request) (any, error) { return hk.Empty{}, nil })
	client, err := hk.Connect(timeout(t), conn, conn, hk.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	err = client.RegisterHook(timeout(t), hk.HookSpec{ID: "prepare", Name: "turn.before"}, func(ctx context.Context, request hk.HookRequest) (hk.HookResponse, error) {
		if !request.IsFirstTurn {
			t.Error("first turn flag missing")
		}
		input := append(request.Input, hk.InputPart{Type: "text", Text: "context"})
		return hk.HookResponse{Input: &input}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var response hk.HookResponse
	err = peer.Call(timeout(t), hk.RequestHookCall, hk.HookRequest{HookID: "prepare", Name: "turn.before", IsFirstTurn: true, Input: []hk.InputPart{{Type: "text", Text: "prompt"}}}, &response)
	if err != nil || response.Input == nil || len(*response.Input) != 2 {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	if err = client.UnregisterHook(timeout(t), "prepare"); err != nil {
		t.Fatal(err)
	}
	if err = peer.Call(timeout(t), hk.RequestHookCall, hk.HookRequest{HookID: "prepare"}, &response); err == nil {
		t.Fatal("unregistered callback remained active")
	}
}
