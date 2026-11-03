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

func copyLimit(v *int64) *int64 {
	if v == nil {
		return nil
	}
	out := max(int64(0), *v)
	return &out
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

// Filter selects events for a subscription. A nil Filter matches all.
type Filter func(Event) bool

// ForSession matches events of one session.
func ForSession(id string) Filter { return func(e Event) bool { return e.SessionID == id } }

// ForTurn matches events of one turn.
func ForTurn(sessionID, turnID string) Filter {
	return func(e Event) bool { return e.SessionID == sessionID && e.TurnID == turnID }
}

// Bus fans events out to subscribers. Publish never blocks: each
// subscriber has a bounded queue, and a subscriber that falls behind
// enters a lagging state in which further events are dropped for it until
// its queue drains. It then receives one StreamGap marker per affected
// session, describing exactly which sequence numbers it missed, before
// normal delivery resumes. Per-session ordering is therefore preserved,
// and loss is always detectable and recoverable from the session Log.
type Bus struct {
	mu         sync.Mutex
	subs       map[*Subscription]struct{}
	max        int
	maxBytes   *int64
	configured bool
}

// NewBus returns an empty bus.
func NewBus() *Bus { return &Bus{subs: make(map[*Subscription]struct{}), max: DefaultBuffer} }

// DefaultBuffer is the per-subscriber queue length used when Subscribe is
// given a non-positive buffer.
const DefaultBuffer = 4096

// Subscribe registers a subscriber. Events matching filter are delivered
// on the returned Subscription's channel C.
func (b *Bus) Subscribe(filter Filter, buffer int) *Subscription {
	b.mu.Lock()
	s := &Subscription{bus: b, filter: filter, requestedMax: buffer,
		max: b.effectiveMax(buffer), maxBytes: copyLimit(b.maxBytes),
		c: make(chan Event), wake: make(chan struct{}, 1), done: make(chan struct{}), gaps: make(map[string]*StreamGapData)}
	s.C = s.c
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	go s.deliver()
	return s
}

func (b *Bus) effectiveMax(requested int) int {
	if requested <= 0 {
		return b.max
	}
	if b.configured {
		return min(requested, b.max)
	}
	return requested
}

// Configure updates limits for current and future subscriptions. An explicit
// subscription count remains a lower local limit. Byte accounting excludes
// one event currently handed to a reader and small recovery gap markers.
func (b *Bus) Configure(maxEvents int, maxBytes *int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.max = max(1, maxEvents)
	b.maxBytes = copyLimit(maxBytes)
	b.configured = true
	for s := range b.subs {
		s.configure(b.effectiveMax(s.requestedMax), b.maxBytes)
	}
}

// Publish offers ev to every matching subscriber without blocking.
func (b *Bus) Publish(ev Event) {
	b.mu.Lock()
	subs := make([]*Subscription, 0, len(b.subs))
	for s := range b.subs {
		subs = append(subs, s)
	}
	b.mu.Unlock()
	for _, s := range subs {
		if s.filter == nil || s.filter(ev) {
			s.offer(ev)
		}
	}
}

// Subscription receives events from a Bus.
type Subscription struct {
	// C delivers events. It is closed after Close.
	C <-chan Event

	bus          *Bus
	filter       Filter
	max          int
	requestedMax int
	maxBytes     *int64
	bytes        int64
	c            chan Event

	mu       sync.Mutex
	queue    []cachedEvent
	lagging  bool
	gaps     map[string]*StreamGapData
	gapOrder []string
	closed   bool

	wake      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func (s *Subscription) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Subscription) recordGap(ev Event) {
	from, to := ev.Seq, ev.Seq
	if ev.Type == StreamGap {
		if d, ok := ev.Data.(StreamGapData); ok {
			from, to = d.FromSeq, d.ToSeq
		} else {
			return
		}
	}
	g, ok := s.gaps[ev.SessionID]
	if !ok {
		g = &StreamGapData{FromSeq: from, ToSeq: to}
		s.gaps[ev.SessionID] = g
		s.gapOrder = append(s.gapOrder, ev.SessionID)
	} else {
		g.FromSeq = min(g.FromSeq, from)
		g.ToSeq = max(g.ToSeq, to)
	}
	s.lagging = true
}

func (s *Subscription) configure(maxEvents int, maxBytes *int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.max, s.maxBytes = maxEvents, copyLimit(maxBytes)
	keep := 0
	var retained int64
	for keep < len(s.queue) && keep < s.max {
		size := s.queue[keep].bytes
		if s.maxBytes != nil && size > *s.maxBytes-retained {
			break
		}
		retained += size
		keep++
	}
	for _, item := range s.queue[keep:] {
		s.recordGap(item.event)
	}
	if keep != len(s.queue) {
		if keep == 0 {
			s.queue = nil
		} else {
			s.queue = append([]cachedEvent(nil), s.queue[:keep]...)
		}
	}
	s.bytes = retained
	s.signal()
}

func (s *Subscription) offer(ev Event) {
	size := EncodedSize(ev)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if size == math.MaxInt64 || s.lagging || len(s.queue) >= s.max || s.maxBytes != nil && size > *s.maxBytes-s.bytes {
		s.recordGap(ev)
		s.mu.Unlock()
		s.signal()
		return
	}
	s.queue = append(s.queue, cachedEvent{ev, size})
	s.bytes += size
	s.mu.Unlock()
	s.signal()
}

func (s *Subscription) deliver() {
	defer close(s.c)
	for {
		s.mu.Lock()
		var next []Event
		switch {
		case len(s.queue) > 0:
			next = []Event{s.queue[0].event}
			s.bytes -= s.queue[0].bytes
			s.queue[0] = cachedEvent{}
			s.queue = s.queue[1:]
			if len(s.queue) == 0 {
				s.queue = nil
			}
		case s.lagging:
			// Queue drained: emit gap markers, then resume.
			for _, sid := range s.gapOrder {
				g := s.gaps[sid]
				next = append(next, Event{SessionID: sid, Type: StreamGap, Time: time.Now().UTC(), Data: *g})
			}
			s.gaps = make(map[string]*StreamGapData)
			s.gapOrder = nil
			s.lagging = false
		}
		s.mu.Unlock()

		if len(next) == 0 {
			select {
			case <-s.wake:
				continue
			case <-s.done:
				return
			}
		}
		for _, ev := range next {
			select {
			case s.c <- ev:
			case <-s.done:
				return
			}
		}
	}
}

// Close unsubscribes and closes C. Undelivered events are discarded.
func (s *Subscription) Close() {
	s.closeOnce.Do(func() {
		s.bus.mu.Lock()
		delete(s.bus.subs, s)
		s.bus.mu.Unlock()
		s.mu.Lock()
		s.closed = true
		s.queue = nil
		s.bytes = 0
		s.gaps = nil
		s.gapOrder = nil
		s.lagging = false
		s.mu.Unlock()
		close(s.done)
	})
}
