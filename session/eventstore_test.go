package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/event"
	"github.com/tomanagle/harness-kit/session"
)

func eventStore(t *testing.T, lastSeq uint64) *session.FileStore {
	t.Helper()
	st, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(context.Background(), session.Record{Info: session.Info{ID: "ses_test", LastSeq: lastSeq}}); err != nil {
		t.Fatal(err)
	}
	return st
}

func persistedEvent(seq uint64) event.Event {
	return event.Event{SessionID: "ses_test", Seq: seq, Type: event.MessageDelta,
		Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Data: event.MessageDeltaData{ItemID: "item", Text: "line one\nline two"}}
}

func TestFileStoreEventsRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := eventStore(t, 0)
	long := strings.Repeat("large output\n", 10000)
	evs := []event.Event{persistedEvent(1), persistedEvent(2), persistedEvent(3)}
	evs[1].Data = event.MessageDeltaData{ItemID: "big", Text: long}
	evs[2].Type, evs[2].Data = "future.event", json.RawMessage(`{"future":true}`)
	for _, ev := range evs {
		if err := st.AppendEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	// Reopening the store must preserve typed payloads and large lines.
	reopened, err := session.NewFileStore(st.Dir())
	if err != nil {
		t.Fatal(err)
	}
	got, truncated, err := reopened.Events(ctx, "ses_test", 0, 0)
	if err != nil || truncated || len(got) != 3 {
		t.Fatalf("events: count=%d truncated=%v err=%v", len(got), truncated, err)
	}
	if got[1].Data.(event.MessageDeltaData).Text != long || string(got[2].Data.(json.RawMessage)) != `{"future":true}` {
		t.Fatalf("payloads did not round trip")
	}
	if !got[0].Time.Equal(evs[0].Time) {
		t.Fatal("event time changed")
	}
	got, truncated, err = reopened.Events(ctx, "ses_test", 1, 1)
	if err != nil || truncated || len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("page: %+v %v %v", got, truncated, err)
	}
	got, truncated, err = reopened.Events(ctx, "ses_test", ^uint64(0), 0)
	if err != nil || truncated || len(got) != 0 {
		t.Fatalf("after end: %+v %v %v", got, truncated, err)
	}
	if list, err := reopened.List(ctx); err != nil || len(list) != 1 {
		t.Fatalf("event file appeared as a record: %+v %v", list, err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(st.Dir(), "ses_test.events.jsonl"))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("event file permissions: %v %v", fi, err)
		}
	}
	if err := reopened.Delete(ctx, "ses_test"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(st.Dir(), "ses_test.events.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("event file survived delete: %v", err)
	}
	if _, _, err := reopened.Events(ctx, "ses_test", 0, 0); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("deleted events: %v", err)
	}
}

func TestFileStoreEventGapsAndLegacyRecords(t *testing.T) {
	ctx := context.Background()
	st := eventStore(t, 5)
	if got, truncated, err := st.Events(ctx, "ses_test", 0, 0); err != nil || len(got) != 0 || !truncated {
		t.Fatalf("legacy: %+v %v %v", got, truncated, err)
	}
	for _, seq := range []uint64{6, 7, 9} {
		if err := st.AppendEvent(ctx, persistedEvent(seq)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		after     uint64
		truncated bool
	}{{0, true}, {5, true}, {7, true}, {8, false}, {9, false}} {
		_, truncated, err := st.Events(ctx, "ses_test", tc.after, 0)
		if err != nil || truncated != tc.truncated {
			t.Fatalf("after %d: truncated=%v err=%v", tc.after, truncated, err)
		}
	}
	if err := st.Put(ctx, session.Record{Info: session.Info{ID: "ses_test", LastSeq: 10}}); err != nil {
		t.Fatal(err)
	}
	if _, truncated, err := st.Events(ctx, "ses_test", 9, 0); err != nil || !truncated {
		t.Fatalf("missing suffix: %v %v", truncated, err)
	}
	if _, _, err := st.Events(ctx, "missing", 0, 0); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestFileStoreRepairsIncompleteEventTail(t *testing.T) {
	ctx := context.Background()
	st := eventStore(t, 0)
	if err := st.AppendEvent(ctx, persistedEvent(1)); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(st.Dir(), "ses_test.events.jsonl")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":2,"sessionId":"ses_test"`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	got, truncated, err := st.Events(ctx, "ses_test", 0, 0)
	if err != nil || truncated || len(got) != 1 {
		t.Fatalf("partial read: %+v %v %v", got, truncated, err)
	}
	if err := st.AppendEvent(ctx, persistedEvent(2)); err != nil {
		t.Fatal(err)
	}
	got, truncated, err = st.Events(ctx, "ses_test", 0, 0)
	if err != nil || truncated || len(got) != 2 || got[1].Seq != 2 {
		t.Fatalf("repaired read: %+v %v %v", got, truncated, err)
	}
}

func TestFileStoreRejectsCorruptEvents(t *testing.T) {
	ctx := context.Background()
	for _, line := range []string{
		"not json\n",
		`{"seq":1,"sessionId":"another","type":"message.delta","data":{}}` + "\n",
		`{"seq":0,"sessionId":"ses_test","type":"message.delta","data":{}}` + "\n",
	} {
		t.Run(line, func(t *testing.T) {
			st := eventStore(t, 0)
			p := filepath.Join(st.Dir(), "ses_test.events.jsonl")
			if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := st.Events(ctx, "ses_test", 0, 0); err == nil {
				t.Fatal("read accepted corruption")
			}
			if err := st.AppendEvent(ctx, persistedEvent(2)); err == nil {
				t.Fatal("append accepted corruption")
			}
			if got, _ := os.ReadFile(p); string(got) != line {
				t.Fatal("corrupt transcript was overwritten")
			}
		})
	}
}

func TestFileStoreEventValidationAndConcurrentReads(t *testing.T) {
	ctx := context.Background()
	st := eventStore(t, 0)
	if err := st.AppendEvent(ctx, persistedEvent(1)); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []event.Event{persistedEvent(0), persistedEvent(1), {SessionID: "../escape", Seq: 2, Type: event.MessageDelta}, {SessionID: "ses_test", Seq: 2, Type: event.StreamGap}} {
		if err := st.AppendEvent(ctx, ev); err == nil {
			t.Fatalf("accepted invalid event: %+v", ev)
		}
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for seq := uint64(2); seq <= 30; seq++ {
			if err := st.AppendEvent(ctx, persistedEvent(seq)); err != nil {
				t.Errorf("append: %v", err)
				return
			}
		}
	}()
	for i := 0; i < 30; i++ {
		got, truncated, err := st.Events(ctx, "ses_test", 0, 0)
		if err != nil || truncated {
			t.Fatalf("concurrent read: %v %v", truncated, err)
		}
		for i, ev := range got {
			if ev.Seq != uint64(i+1) {
				t.Fatalf("out of order: %+v", got)
			}
		}
	}
	wg.Wait()
}
