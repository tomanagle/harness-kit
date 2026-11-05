package event

import (
	"encoding/json"
	"math"
	"sync"
	"time"
)

// Log is a bounded, append-only record of one session's events. Limits
// account for encoded JSON bytes, not exact heap usage or process RSS.
// Log is safe for concurrent use. Its backing storage grows on demand.
type Log struct {
	mu                 sync.Mutex
	buf                []cachedEvent
	start, n, capacity int
	bytes              int64
	maxBytes           *int64
	lastSeq            uint64
	now                func() time.Time
}

type cachedEvent struct {
	event Event
	bytes int64
}

// EncodedSize estimates retained payload bytes using the event's JSON encoding.
// An unencodable event is treated as too large for any finite byte budget.
func EncodedSize(ev Event) int64 {
	b, err := json.Marshal(ev)
	if err != nil {
		return math.MaxInt64
	}
	return int64(len(b))
}

// NewLog returns a log retaining at most capacity events (minimum 1).
func NewLog(capacity int) *Log {
	if capacity < 1 {
		capacity = 1
	}
	return &Log{capacity: capacity, now: time.Now}
}

// NewLogWithHistory restores a bounded tail of ordered events. Sequence
// numbers and timestamps are preserved, including the record high-water mark.
func NewLogWithHistory(capacity int, history []Event, lastSeq uint64) *Log {
	l := NewLog(capacity)
	l.lastSeq = lastSeq
	for _, ev := range history {
		l.Restore(ev)
	}
	return l
}

// Configure changes count and byte limits, discarding oldest events as needed.
// A nil byte limit is unlimited; zero retains no events. Capacity zero also
// disables caching. Neither changing limits nor Clear resets LastSeq.
func (l *Log) Configure(capacity int, maxBytes *int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	capacity = max(0, capacity)
	limit := copyLimit(maxBytes)
	if l.capacity == capacity && (l.maxBytes == nil && limit == nil || l.maxBytes != nil && limit != nil && *l.maxBytes == *limit) {
		return
	}
	l.capacity = capacity
	l.maxBytes = limit
	for l.n > l.capacity || l.maxBytes != nil && l.bytes > *l.maxBytes {
		l.dropOldest()
	}
	l.compact(l.n)
}

// Clear releases every cached event and its backing storage.
func (l *Log) Clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = nil
	l.start, l.n, l.bytes = 0, 0, 0
}

// Stats reports retained event count and encoded JSON bytes.
func (l *Log) Stats() (events int, bytes int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n, l.bytes
}

// Restore adds an already assigned event, preserving its sequence and time.
// Callers supply events in order. It is useful for bounded streaming restore.
func (l *Log) Restore(ev Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastSeq = max(l.lastSeq, ev.Seq)
	l.retain(ev)
}

// Append assigns a sequence and time, then records and returns the event.
func (l *Log) Append(ev Event) Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastSeq++
	ev.Seq = l.lastSeq
	if ev.Time.IsZero() {
		ev.Time = l.now().UTC()
	}
	l.retain(ev)
	return ev
}

func (l *Log) retain(ev Event) {
	size := EncodedSize(ev)
	if size == math.MaxInt64 || l.capacity == 0 || l.maxBytes != nil && size > *l.maxBytes {
		return
	}
	for l.n > 0 && (l.n >= l.capacity || l.maxBytes != nil && size > *l.maxBytes-l.bytes) {
		l.dropOldest()
	}
	if l.n == len(l.buf) {
		l.compact(min(l.capacity, max(8, 2*len(l.buf))))
	}
	l.buf[(l.start+l.n)%len(l.buf)] = cachedEvent{ev, size}
	l.n++
	l.bytes += size
}

func (l *Log) dropOldest() {
	if l.n == 0 {
		return
	}
	l.bytes -= l.buf[l.start].bytes
	l.buf[l.start] = cachedEvent{}
	l.start = (l.start + 1) % len(l.buf)
	l.n--
}

func (l *Log) compact(capacity int) {
	if capacity == 0 {
		l.buf = nil
		l.start = 0
		return
	}
	next := make([]cachedEvent, capacity)
	for i := 0; i < l.n; i++ {
		next[i] = l.buf[(l.start+i)%len(l.buf)]
	}
	l.buf, l.start = next, 0
}

// LastSeq returns the newest assigned sequence (0 if none).
func (l *Log) LastSeq() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastSeq
}

// Since returns up to limit events with Seq > afterSeq (limit <= 0 means
// no limit). truncated reports requested events missing from the cache.
func (l *Log) Since(afterSeq uint64, limit int) (events []Event, truncated bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n == 0 {
		return nil, afterSeq < l.lastSeq
	}
	oldest := l.buf[l.start].event.Seq
	truncated = oldest > 0 && afterSeq < oldest-1
	for i := 0; i < l.n; i++ {
		ev := l.buf[(l.start+i)%len(l.buf)].event
		if ev.Seq <= afterSeq {
			continue
		}
		if len(events) == 0 && ev.Seq-afterSeq > 1 || len(events) > 0 && ev.Seq != events[len(events)-1].Seq+1 {
			truncated = true
		}
		events = append(events, ev)
		if limit > 0 && len(events) == limit {
			break
		}
	}
	if (limit <= 0 || len(events) < limit) && afterSeq < l.lastSeq && (len(events) == 0 || events[len(events)-1].Seq < l.lastSeq) {
		truncated = true
	}
	return events, truncated
}
