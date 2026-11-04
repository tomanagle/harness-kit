package event

import (
	"strings"
	"testing"
	"time"
)

func memoryEvent(seq uint64, text string) Event {
	return Event{SessionID: "s", Seq: seq, Type: MessageDelta,
		Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Data: MessageDeltaData{Text: text}}
}

func TestLogLiveByteLimitsAndClearPreserveSequence(t *testing.T) {
	log := NewLog(100)
	for i := 0; i < 5; i++ {
		log.Append(memoryEvent(0, "hello"))
	}
	one := EncodedSize(memoryEvent(5, "hello"))
	log.Configure(100, &one)
	if n, bytes := log.Stats(); n != 1 || bytes != one {
		t.Fatalf("resized log: %d events, %d bytes", n, bytes)
	}
	if got, gap := log.Since(0, 0); !gap || len(got) != 1 || got[0].Seq != 5 {
		t.Fatalf("resized tail: %+v, gap %v", got, gap)
	}
	log.Append(memoryEvent(0, strings.Repeat("x", 1000)))
	log.Append(memoryEvent(0, "hello"))
	if got, gap := log.Since(5, 0); !gap || len(got) != 1 || got[0].Seq != 7 {
		t.Fatalf("oversize gap: %+v, gap %v", got, gap)
	}
	log.Clear()
	if n, bytes := log.Stats(); n != 0 || bytes != 0 || log.LastSeq() != 7 {
		t.Fatalf("clear: %d %d last=%d", n, bytes, log.LastSeq())
	}
	if ev := log.Append(memoryEvent(0, "hello")); ev.Seq != 8 {
		t.Fatalf("sequence reset: %+v", ev)
	}
	zero := int64(0)
	log.Configure(100, &zero)
	log.Append(memoryEvent(0, "hello"))
	if n, bytes := log.Stats(); n != 0 || bytes != 0 || log.LastSeq() != 9 {
		t.Fatalf("zero budget: %d %d last=%d", n, bytes, log.LastSeq())
	}
	log.Configure(2, nil)
	log.Append(memoryEvent(0, "hello"))
	if n, _ := log.Stats(); n != 1 {
		t.Fatal("cache did not resume after budget increased")
	}
}

func TestStreamingLogRestoreKeepsBoundedTail(t *testing.T) {
	log := NewLogWithHistory(3, nil, 2000)
	budget := EncodedSize(memoryEvent(1000, "hello")) * 2
	log.Configure(3, &budget)
	for i := uint64(1); i <= 1000; i++ {
		log.Restore(memoryEvent(i, "hello"))
		if n, bytes := log.Stats(); n > 3 || bytes > budget {
			t.Fatalf("restore exceeded limits: %d events %d bytes", n, bytes)
		}
	}
	tail, gap := log.Since(0, 0)
	if !gap || len(tail) != 2 || tail[0].Seq != 999 || tail[1].Seq != 1000 {
		t.Fatalf("tail: %+v, gap %v", tail, gap)
	}
	if ev := log.Append(memoryEvent(0, "next")); ev.Seq != 2001 {
		t.Fatalf("lost record high-water mark: %+v", ev)
	}
}

func TestBusByteLimitReportsRecoverableGap(t *testing.T) {
	bus := NewBus()
	budget := EncodedSize(memoryEvent(20, "hello")) * 2
	bus.Configure(100, &budget)
	sub := bus.Subscribe(nil, 0)
	defer sub.Close()
	for i := uint64(1); i <= 20; i++ {
		bus.Publish(memoryEvent(i, "hello"))
	}
	var last uint64
	for {
		ev := recv(t, sub)
		if ev.Type == StreamGap {
			gap := ev.Data.(StreamGapData)
			if gap.FromSeq != last+1 || gap.ToSeq != 20 || last > 3 {
				t.Fatalf("gap %+v after %d events", gap, last)
			}
			break
		}
		if ev.Seq != last+1 {
			t.Fatalf("delivery out of order: %+v after %d", ev, last)
		}
		last = ev.Seq
	}
	bus.Configure(100, nil)
	bus.Publish(memoryEvent(21, "hello"))
	if got := recv(t, sub); got.Seq != 21 {
		t.Fatalf("did not recover: %+v", got)
	}
}

func TestBusLiveResizeDropsSuffixInOrder(t *testing.T) {
	bus := NewBus()
	sub := bus.Subscribe(nil, 0)
	defer sub.Close()
	for i := uint64(1); i <= 20; i++ {
		bus.Publish(memoryEvent(i, "hello"))
	}
	bus.Configure(2, nil)
	var last uint64
	for {
		ev := recv(t, sub)
		if ev.Type == StreamGap {
			gap := ev.Data.(StreamGapData)
			if gap.FromSeq != last+1 || gap.ToSeq != 20 || last > 3 {
				t.Fatalf("resize gap %+v after %d", gap, last)
			}
			break
		}
		if ev.Seq != last+1 {
			t.Fatalf("resize reordered delivery: %+v after %d", ev, last)
		}
		last = ev.Seq
	}
}

func TestZeroQueueBytesStillWakesGapDelivery(t *testing.T) {
	bus := NewBus()
	zero := int64(0)
	bus.Configure(10, &zero)
	sub := bus.Subscribe(nil, 0)
	defer sub.Close()
	bus.Publish(memoryEvent(1, "hello"))
	got := recv(t, sub)
	if got.Type != StreamGap || got.Data.(StreamGapData).FromSeq != 1 {
		t.Fatalf("zero-byte queue: %+v", got)
	}
}
