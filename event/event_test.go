package event

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/permission"
)

func TestEventJSONRoundTrip(t *testing.T) {
	cost := 0.25
	in := []Event{
		{Seq: 1, SessionID: "s", Type: MessageDelta, Data: MessageDeltaData{ItemID: "i", Text: "hi"}},
		{Seq: 2, SessionID: "s", TurnID: "t", Type: TurnCompleted, Data: TurnCompletedData{Status: TurnStatusCompleted, Usage: &UsageData{Scope: "turn", InputTokens: 3, CostUSD: &cost}}},
		{Seq: 3, SessionID: "s", Type: ApprovalRequested, Data: permission.Request{ID: "a1", Kind: permission.KindCommand, Title: "run ls"}},
		{Seq: 4, SessionID: "s", Type: "future.type", Data: map[string]int{"x": 1}},
		{Seq: 5, SessionID: "s", Type: SessionClosed},
	}
	for _, ev := range in {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		var out Event
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		if out.Seq != ev.Seq || out.Type != ev.Type || out.TurnID != ev.TurnID {
			t.Fatalf("envelope mismatch: %+v vs %+v", out, ev)
		}
		switch d := out.Data.(type) {
		case MessageDeltaData:
			if d.Text != "hi" {
				t.Fatalf("delta: %+v", d)
			}
		case TurnCompletedData:
			if d.Status != TurnStatusCompleted || d.Usage == nil || *d.Usage.CostUSD != 0.25 {
				t.Fatalf("turn: %+v", d)
			}
		case permission.Request:
			if d.ID != "a1" || d.Kind != permission.KindCommand {
				t.Fatalf("approval: %+v", d)
			}
		case json.RawMessage:
			if ev.Type != "future.type" || string(d) != `{"x":1}` {
				t.Fatalf("raw: %s", d)
			}
		case nil:
			if ev.Type != SessionClosed {
				t.Fatalf("unexpected nil data for %s", ev.Type)
			}
		default:
			t.Fatalf("unexpected payload %T", d)
		}
	}
}

func TestLogSequenceAndEviction(t *testing.T) {
	l := NewLog(3)
	for i := 0; i < 5; i++ {
		ev := l.Append(Event{SessionID: "s", Type: MessageDelta})
		if ev.Seq != uint64(i+1) || ev.Time.IsZero() {
			t.Fatalf("append %d: %+v", i, ev)
		}
	}
	evs, truncated := l.Since(0, 0)
	if !truncated || len(evs) != 3 || evs[0].Seq != 3 || evs[2].Seq != 5 {
		t.Fatalf("since 0: %v %+v", truncated, evs)
	}
	evs, truncated = l.Since(3, 0)
	if truncated || len(evs) != 2 || evs[0].Seq != 4 {
		t.Fatalf("since 3: %v %+v", truncated, evs)
	}
	evs, _ = l.Since(2, 1)
	if len(evs) != 1 || evs[0].Seq != 3 {
		t.Fatalf("limit: %+v", evs)
	}
	if evs, truncated := l.Since(5, 0); len(evs) != 0 || truncated {
		t.Fatalf("up to date: %v %+v", truncated, evs)
	}
	if l.LastSeq() != 5 {
		t.Fatal("last seq")
	}
}

func recv(t *testing.T, s *Subscription) Event {
	t.Helper()
	select {
	case ev, ok := <-s.C:
		if !ok {
			t.Fatal("subscription closed")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for event")
	}
	return Event{}
}

func TestBusFilterAndOrder(t *testing.T) {
	b := NewBus()
	all := b.Subscribe(nil, 0)
	only := b.Subscribe(ForSession("b"), 0)
	defer all.Close()
	defer only.Close()

	la, lb := NewLog(10), NewLog(10)
	for i := 0; i < 3; i++ {
		b.Publish(la.Append(Event{SessionID: "a", Type: MessageDelta}))
		b.Publish(lb.Append(Event{SessionID: "b", Type: MessageDelta}))
	}
	for i := 0; i < 6; i++ {
		ev := recv(t, all)
		wantSession := "a"
		if i%2 == 1 {
			wantSession = "b"
		}
		if ev.SessionID != wantSession || ev.Seq != uint64(i/2+1) {
			t.Fatalf("all[%d] = %s/%d", i, ev.SessionID, ev.Seq)
		}
	}
	for i := 0; i < 3; i++ {
		if ev := recv(t, only); ev.SessionID != "b" || ev.Seq != uint64(i+1) {
			t.Fatalf("only[%d] = %+v", i, ev)
		}
	}
}

func TestBusLaggingSubscriberGetsGap(t *testing.T) {
	b := NewBus()
	s := b.Subscribe(nil, 2)
	defer s.Close()
	l := NewLog(100)

	// The delivery goroutine may move at most one event into the channel
	// hand-off; publish enough that the queue certainly overflows.
	for i := 0; i < 20; i++ {
		b.Publish(l.Append(Event{SessionID: "s", Type: MessageDelta}))
	}

	var seqs []uint64
	var gap *StreamGapData
	for gap == nil {
		ev := recv(t, s)
		if ev.Type == StreamGap {
			g := ev.Data.(StreamGapData)
			gap = &g
			break
		}
		seqs = append(seqs, ev.Seq)
	}
	if len(seqs) == 0 || len(seqs) > 3 {
		t.Fatalf("expected 1..3 delivered events before gap, got %v", seqs)
	}
	for i, seq := range seqs {
		if seq != uint64(i+1) {
			t.Fatalf("delivered out of order: %v", seqs)
		}
	}
	if gap.FromSeq != seqs[len(seqs)-1]+1 || gap.ToSeq != 20 {
		t.Fatalf("gap = %+v after %v", gap, seqs)
	}

	// Delivery resumes normally after the gap.
	b.Publish(l.Append(Event{SessionID: "s", Type: MessageDelta}))
	if ev := recv(t, s); ev.Seq != 21 {
		t.Fatalf("after gap: %+v", ev)
	}

	// Recovery: the log still has the dropped events.
	missed, truncated := l.Since(gap.FromSeq-1, int(gap.ToSeq-gap.FromSeq+1))
	if truncated || uint64(len(missed)) != gap.ToSeq-gap.FromSeq+1 {
		t.Fatalf("recovery: %v %d", truncated, len(missed))
	}
}

func TestSubscriptionClose(t *testing.T) {
	b := NewBus()
	s := b.Subscribe(nil, 0)
	s.Close()
	s.Close() // idempotent
	b.Publish(Event{SessionID: "s"})
	select {
	case _, ok := <-s.C:
		if ok {
			t.Fatal("expected closed channel")
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed")
	}
}

func TestRestoredLogPreservesHistoryAndGaps(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	history := []Event{{Seq: 1, Time: at}, {Seq: 3, Time: at}, {Seq: 4, Time: at}}
	log := NewLogWithHistory(3, history, 6)
	page, truncated := log.Since(1, 1)
	if len(page) != 1 || page[0].Seq != 3 || !truncated || !page[0].Time.Equal(at) {
		t.Fatalf("missing event before page: %+v %v", page, truncated)
	}
	if tail, truncated := log.Since(4, 0); len(tail) != 0 || !truncated {
		t.Fatalf("missing recorded suffix: %+v %v", tail, truncated)
	}
	if ev := log.Append(Event{Type: MessageDelta}); ev.Seq != 7 {
		t.Fatalf("record high-water mark lost: %+v", ev)
	}
	log = NewLogWithHistory(1, history, 2)
	if ev := log.Append(Event{Type: MessageDelta}); ev.Seq != 5 {
		t.Fatalf("durable high-water mark lost: %+v", ev)
	}
	if _, truncated := log.Since(^uint64(0), 0); truncated {
		t.Fatal("cursor beyond end must not overflow")
	}
}

func TestSessionUpdatedPayloadRoundTrip(t *testing.T) {
	in := Event{Type: SessionUpdated, Data: SessionUpdatedData{Title: "Renamed", State: "idle", Turns: 2, LastStatus: TurnStatusCompleted}}
	encoded, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Event
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	data, ok := decoded.Data.(SessionUpdatedData)
	if !ok || data.Title != "Renamed" || data.State != "idle" || data.Turns != 2 || data.ActiveTurnID != "" {
		t.Fatalf("updated payload: %#v", decoded.Data)
	}
}
