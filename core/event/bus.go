package event

import (
	"math"
	"sync"
	"time"
)

func copyLimit(v *int64) *int64 {
	if v == nil {
		return nil
	}
	out := max(int64(0), *v)
	return &out
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
