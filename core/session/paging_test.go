package session_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/session"
)

func TestFileStorePagesRespectCountAndByteLimits(t *testing.T) {
	ctx := context.Background()
	store := eventStore(t, 0)
	for seq := uint64(1); seq <= 7; seq++ {
		if err := store.AppendEvent(ctx, persistedEvent(seq)); err != nil {
			t.Fatal(err)
		}
	}
	budget := event.EncodedSize(persistedEvent(1)) * 2
	var cursor uint64
	var seen []uint64
	for {
		page, err := store.EventsPage(ctx, "ses_test", cursor, 3, &budget)
		if err != nil || page.Truncated {
			t.Fatalf("page: %+v %v", page, err)
		}
		var bytes int64
		for _, ev := range page.Events {
			bytes += event.EncodedSize(ev)
			seen = append(seen, ev.Seq)
		}
		if len(page.Events) > 2 || bytes > budget {
			t.Fatalf("page exceeded budget: count %d bytes %d", len(page.Events), bytes)
		}
		if page.NextSeq != seen[len(seen)-1] {
			t.Fatalf("cursor: %+v", page)
		}
		cursor = page.NextSeq
		if !page.HasMore {
			break
		}
	}
	if len(seen) != 7 {
		t.Fatalf("missing events: %v", seen)
	}
	for i, seq := range seen {
		if seq != uint64(i+1) {
			t.Fatalf("sequence: %v", seen)
		}
	}
	page, err := store.EventsPage(ctx, "ses_test", 7, 3, &budget)
	if err != nil || page.HasMore || page.Truncated || page.NextSeq != 7 || len(page.Events) != 0 {
		t.Fatalf("end: %+v %v", page, err)
	}
}

func TestFileStorePageOversizeNeverAdvancesCursor(t *testing.T) {
	ctx := context.Background()
	store := eventStore(t, 0)
	for seq := uint64(1); seq <= 3; seq++ {
		ev := persistedEvent(seq)
		if seq == 2 {
			ev.Data = event.MessageDeltaData{Text: strings.Repeat("x", 10000)}
		}
		if err := store.AppendEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	budget := int64(1000)
	first, err := store.EventsPage(ctx, "ses_test", 0, 100, &budget)
	if err != nil || len(first.Events) != 1 || first.NextSeq != 1 || !first.HasMore {
		t.Fatalf("before oversize: %+v %v", first, err)
	}
	blocked, err := store.EventsPage(ctx, "ses_test", first.NextSeq, 100, &budget)
	if !errors.Is(err, session.ErrEventTooLarge) || blocked.NextSeq != 1 || len(blocked.Events) != 0 {
		t.Fatalf("oversize: %+v %v", blocked, err)
	}
	resumed, err := store.EventsPage(ctx, "ses_test", first.NextSeq, 1, nil)
	if err != nil || len(resumed.Events) != 1 || resumed.NextSeq != 2 || !resumed.HasMore {
		t.Fatalf("larger budget: %+v %v", resumed, err)
	}
}

func TestFileStorePageDistinguishesGapsFromPagination(t *testing.T) {
	ctx := context.Background()
	store := eventStore(t, 9)
	for _, seq := range []uint64{2, 3, 5} {
		if err := store.AppendEvent(ctx, persistedEvent(seq)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		after           uint64
		limit           int
		truncated, more bool
	}{{0, 1, true, true}, {2, 1, false, true}, {3, 1, true, false}, {5, 1, true, false}, {9, 1, false, false}} {
		page, err := store.EventsPage(ctx, "ses_test", tc.after, tc.limit, nil)
		if err != nil || page.Truncated != tc.truncated || page.HasMore != tc.more {
			t.Fatalf("after %d: %+v %v", tc.after, page, err)
		}
	}
}

func TestFileStoreStreamingRestoreAndTurnLookup(t *testing.T) {
	ctx := context.Background()
	store := eventStore(t, 0)
	for seq := uint64(1); seq <= 150; seq++ {
		ev := persistedEvent(seq)
		if seq == 30 {
			ev.TurnID = "old_turn"
		}
		if err := store.AppendEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	log := event.NewLog(2)
	if err := store.ScanEvents(ctx, "ses_test", func(ev event.Event) error {
		log.Restore(ev)
		if n, _ := log.Stats(); n > 2 {
			t.Fatal("streaming restore grew beyond tail")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tail, truncated := log.Since(0, 0)
	if !truncated || len(tail) != 2 || tail[0].Seq != 149 || log.LastSeq() != 150 {
		t.Fatalf("tail %+v truncated %v", tail, truncated)
	}
	for _, id := range []string{"old_turn", "unknown", ""} {
		found, err := store.HasTurn(ctx, "ses_test", id)
		if err != nil || found != (id == "old_turn") {
			t.Fatalf("lookup %q: %v %v", id, found, err)
		}
	}
	stop := errors.New("caller stopped")
	count := 0
	if err := store.ScanEvents(ctx, "ses_test", func(event.Event) error {
		count++
		return stop
	}); !errors.Is(err, stop) || count != 1 {
		t.Fatalf("early stop: count %d err %v", count, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.ScanEvents(cancelled, "ses_test", func(event.Event) error { t.Fatal("visited after cancellation"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}
