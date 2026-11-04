package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
)

func TestNativeCompactionWaitsForCompletion(t *testing.T) {
	for _, mode := range []string{"", "before-ack"} {
		t.Run(mode, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "requests.jsonl")
			rt := newFake(t, "HK_FAKE_CODEX_COMPACT="+mode, "HK_FAKE_CODEX_REQUEST_LOG="+log)
			rec := &recorder{}
			live, err := rt.Start(context.Background(), agent.SessionConfig{}, rec)
			if err != nil {
				t.Fatal(err)
			}
			defer live.Close(context.Background())
			compact := live.(agent.Compactor)
			if !compact.CanCompact() {
				t.Fatal("native compaction not advertised")
			}
			if err := compact.Compact(context.Background()); err != nil {
				t.Fatal(err)
			}
			usage := rec.ofType(event.ContextUsage)
			if len(usage) != 2 {
				t.Fatalf("context lifecycle: %+v", usage)
			}
			before := usage[0].Data.(event.ContextUsageData)
			after := usage[1].Data.(event.ContextUsageData)
			if before.UsedTokens != nil || !before.Stale || after.UsedTokens == nil || *after.UsedTokens != 300 || after.WindowTokens == nil || *after.WindowTokens != 200000 || after.Basis != "last_request" || after.Stale {
				t.Fatalf("context before/after: %+v %+v", before, after)
			}
			spend := rec.ofType(event.Usage)
			if len(spend) != 1 || spend[0].Data.(event.UsageData).TotalTokens != 11000 {
				t.Fatalf("context polluted cumulative accounting: %+v", spend)
			}
			requests, err := os.ReadFile(log)
			if err != nil || !strings.Contains(string(requests), `"thread/compact/start"`) || strings.Contains(string(requests), `"turn/start"`) {
				t.Fatalf("compaction must not send a user prompt: %s %v", requests, err)
			}
			if len(rec.ofType(event.MessageDelta)) != 0 {
				t.Fatal("manual compaction produced synthetic chat output")
			}
		})
	}
}

func TestNativeCompactionCancellationUsesObservedTurnID(t *testing.T) {
	for _, mode := range []string{"slow", "delayed-start"} {
		t.Run(mode, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "requests.jsonl")
			live, err := newFake(t, "HK_FAKE_CODEX_COMPACT="+mode, "HK_FAKE_CODEX_REQUEST_LOG="+log).Start(context.Background(), agent.SessionConfig{}, &recorder{})
			if err != nil {
				t.Fatal(err)
			}
			defer live.Close(context.Background())
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if err := live.(agent.Compactor).Compact(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cancellation: %v", err)
			}
			requests, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, line := range strings.Split(string(requests), "\n") {
				var request struct {
					Method string
					Params struct{ ThreadID, TurnID string }
				}
				_ = json.Unmarshal([]byte(line), &request)
				if request.Method == "turn/interrupt" {
					found = true
					if request.Params.ThreadID != "thr_new" || request.Params.TurnID != "compact_1" {
						t.Fatalf("interrupt used wrong identity: %+v", request.Params)
					}
				}
			}
			if !found {
				t.Fatal("compaction cancellation never interrupted the native turn")
			}
			result, err := live.Prompt(context.Background(), agent.Turn{ID: "after", Input: agent.Text("hello")})
			if err != nil || result.Status != event.TurnStatusCompleted {
				t.Fatalf("session unusable after confirmed cancellation: %+v %v", result, err)
			}
		})
	}
}

func TestNativeCompactionFailureAndUnsupported(t *testing.T) {
	for _, mode := range []string{"fail", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			live, err := newFake(t, "HK_FAKE_CODEX_COMPACT="+mode).Start(context.Background(), agent.SessionConfig{}, &recorder{})
			if err != nil {
				t.Fatal(err)
			}
			defer live.Close(context.Background())
			compact := live.(agent.Compactor)
			err = compact.Compact(context.Background())
			if err == nil {
				t.Fatal("native failure returned success")
			}
			if mode == "unsupported" && (!errors.Is(err, agent.ErrUnsupported) || compact.CanCompact()) {
				t.Fatalf("unsupported capability not reflected: %v", err)
			}
			result, err := live.Prompt(context.Background(), agent.Turn{ID: "after", Input: agent.Text("hello")})
			if err != nil || result.Status != event.TurnStatusCompleted {
				t.Fatalf("failure left operation slot occupied: %+v %v", result, err)
			}
		})
	}
}

func TestNativeCompactionReservesOperationUntilCancelled(t *testing.T) {
	rec := &recorder{}
	live, err := newFake(t, "HK_FAKE_CODEX_COMPACT=slow").Start(context.Background(), agent.SessionConfig{}, rec)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	compact := live.(agent.Compactor)
	done := make(chan error, 1)
	go func() { done <- compact.Compact(ctx) }()
	started := func() bool {
		for _, ev := range rec.ofType(event.RuntimeStatus) {
			if ev.Data.(event.RuntimeStatusData).Status == "compacting" {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(3 * time.Second)
	for !started() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !started() {
		t.Fatal("compaction did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("returned on acknowledgement before native completion: %v", err)
	default:
	}
	if _, err := live.Prompt(context.Background(), agent.Turn{ID: "overlap", Input: agent.Text("hello")}); err == nil {
		t.Fatal("prompt overlapped compaction")
	}
	if err := compact.Compact(context.Background()); err == nil {
		t.Fatal("second compaction overlapped the first")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestContextUsageDoesNotUseCumulativeSpendOrInventUnknownZero(t *testing.T) {
	for _, test := range []struct {
		name, usage string
		used        *int64
	}{
		{"last", `{"total":{"totalTokens":999999},"last":{"totalTokens":37},"modelContextWindow":200000}`, int64Value(37)},
		{"zero", `{"total":{"totalTokens":999999},"last":{"totalTokens":0},"modelContextWindow":200000}`, int64Value(0)},
		{"absent", `{"total":{"totalTokens":999999},"modelContextWindow":200000}`, nil},
		{"empty", `{"last":{},"modelContextWindow":200000}`, nil},
		{"negative", `{"last":{"totalTokens":-1},"modelContextWindow":200000}`, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := &recorder{}
			s := &Session{h: rec, threadID: "thread"}
			s.onNotification("thread/tokenUsage/updated", json.RawMessage(`{"threadId":"thread","tokenUsage":`+test.usage+`}`))
			updates := rec.ofType(event.ContextUsage)
			if len(updates) != 1 {
				t.Fatalf("updates: %+v", updates)
			}
			got := updates[0].Data.(event.ContextUsageData)
			if got.WindowTokens == nil || *got.WindowTokens != 200000 || (got.UsedTokens == nil) != (test.used == nil) || (test.used != nil && *got.UsedTokens != *test.used) {
				t.Fatalf("context: %+v", got)
			}
			if test.used == nil && got.Basis != "unknown" {
				t.Fatalf("unknown measurement: %+v", got)
			}
			s.onNotification("thread/compacted", json.RawMessage(`{"threadId":"thread"}`))
			updates = rec.ofType(event.ContextUsage)
			stale := updates[len(updates)-1].Data.(event.ContextUsageData)
			if stale.UsedTokens != nil || !stale.Stale {
				t.Fatalf("legacy compaction retained old occupancy: %+v", stale)
			}
		})
	}
}

func int64Value(value int64) *int64 { return &value }

func TestNativeCompactionStartPreservesHostBeforeSnapshot(t *testing.T) {
	rec := &recorder{}
	s := &Session{h: rec, threadID: "thread"}
	s.onNotification("thread/tokenUsage/updated", json.RawMessage(`{"threadId":"thread","tokenUsage":{"last":{"totalTokens":37},"modelContextWindow":200000}}`))
	s.onNotification("item/started", json.RawMessage(`{"threadId":"thread","turnId":"native","item":{"id":"compact","type":"contextCompaction"}}`))
	var latest event.ContextUsageData
	started := false
	for _, ev := range rec.events {
		if ev.Type == event.ContextUsage {
			latest = ev.Data.(event.ContextUsageData)
		}
		if status, ok := ev.Data.(event.RuntimeStatusData); ok && status.Status == "compacting" {
			started = true
			if latest.UsedTokens == nil || *latest.UsedTokens != 37 || latest.Stale {
				t.Fatalf("compaction start lost the host's Before observation: %+v", latest)
			}
		}
	}
	if !started || latest.UsedTokens != nil || !latest.Stale {
		t.Fatalf("compaction start must then invalidate old occupancy: started=%v latest=%+v", started, latest)
	}
}
