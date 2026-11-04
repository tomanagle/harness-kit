// Package logging connects Go's slog diagnostics to bounded live subscriptions.
// It does not replace the global logger, retain history, or write to disk.
package logging

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Config bounds each subscription's queued records and each normalized record.
// A subscriber may additionally hold one in-flight delivery and one gap marker.
type Config struct {
	MaxQueueRecords int
	MaxQueueBytes   int64
	MaxRecordBytes  int
}

func DefaultConfig() Config {
	return Config{MaxQueueRecords: 256, MaxQueueBytes: 1 << 20, MaxRecordBytes: 64 << 10}
}
func defaults(c Config) Config {
	d := DefaultConfig()
	if c.MaxQueueRecords <= 0 {
		c.MaxQueueRecords = d.MaxQueueRecords
	}
	if c.MaxQueueBytes <= 0 {
		c.MaxQueueBytes = d.MaxQueueBytes
	}
	if c.MaxRecordBytes <= 0 {
		c.MaxRecordBytes = d.MaxRecordBytes
	}
	return c
}

type Source struct {
	Function string `json:"function"`
	File     string `json:"file"`
	Line     int    `json:"line"`
}

// Record is one normalized slog record. Attrs preserve nested groups and JSON
// value types; keys such as "message" in attrs cannot replace envelope fields.
type Record struct {
	Seq       uint64                     `json:"seq"`
	Time      time.Time                  `json:"time"`
	Level     string                     `json:"level"`
	Message   string                     `json:"message"`
	Attrs     map[string]json.RawMessage `json:"attrs"`
	Source    *Source                    `json:"source,omitempty"`
	Truncated bool                       `json:"truncated,omitempty"`
}

// Gap describes dropped live records. There is no retained history to replay.
type Gap struct {
	FromSeq uint64 `json:"fromSeq"`
	ToSeq   uint64 `json:"toSeq"`
	Count   uint64 `json:"count"`
}

// Notification carries exactly one record or dropped-record marker.
type Notification struct {
	Record  *Record `json:"record,omitempty"`
	Dropped *Gap    `json:"dropped,omitempty"`
}

// Stream is a slog.Handler with independent, nonblocking live subscriptions.
// Level controls the stream; an optional next handler keeps its own level policy.
type Stream struct {
	mu      sync.Mutex
	config  Config
	level   *slog.LevelVar
	seq     uint64
	closed  bool
	subs    map[*Subscription]struct{}
	handler *handler
}

// New creates a stream. Zero config fields use DefaultConfig. Supply the same
// LevelVar to an existing handler when both destinations should share a level.
func New(level *slog.LevelVar, next slog.Handler, config Config) *Stream {
	if level == nil {
		level = new(slog.LevelVar)
	}
	s := &Stream{config: defaults(config), level: level, subs: make(map[*Subscription]struct{})}
	// Keeping user attributes under attrs avoids collisions with slog metadata.
	j := slog.NewJSONHandler(recordWriter{s}, &slog.HandlerOptions{Level: level, AddSource: true}).WithGroup("attrs")
	s.handler = &handler{stream: s, json: j, next: next}
	return s
}
func (s *Stream) Level() *slog.LevelVar { return s.level }
func (s *Stream) Config() Config        { s.mu.Lock(); defer s.mu.Unlock(); return s.config }
func (s *Stream) Configure(config Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config = defaults(config)
	for sub := range s.subs {
		sub.configure(s.config)
	}
}
func (s *Stream) Enabled(ctx context.Context, level slog.Level) bool {
	return s.handler.Enabled(ctx, level)
}
func (s *Stream) Handle(ctx context.Context, r slog.Record) error { return s.handler.Handle(ctx, r) }
func (s *Stream) WithAttrs(attrs []slog.Attr) slog.Handler        { return s.handler.WithAttrs(attrs) }
func (s *Stream) WithGroup(name string) slog.Handler              { return s.handler.WithGroup(name) }

// WithDestination adds a handler without replacing the stream's existing
// downstream destination. Each destination keeps its own Enabled policy.
// A handler already derived from this stream is returned unchanged, preserving
// its attributes and groups without duplicating stream delivery.
func (s *Stream) WithDestination(next slog.Handler) slog.Handler {
	if next == nil {
		return s
	}
	switch h := next.(type) {
	case *Stream:
		if h == s {
			return next
		}
	case *handler:
		if h.stream == s {
			return next
		}
	case *destinationHandler:
		if h.stream == s {
			return next
		}
	}
	return &destinationHandler{stream: s, primary: s, next: next}
}

type destinationHandler struct {
	stream        *Stream
	primary, next slog.Handler
}

func (h *destinationHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.primary.Enabled(ctx, level) || h.next.Enabled(ctx, level)
}
func (h *destinationHandler) Handle(ctx context.Context, r slog.Record) error {
	var err error
	if h.primary.Enabled(ctx, r.Level) {
		err = h.primary.Handle(ctx, r.Clone())
	}
	if h.next.Enabled(ctx, r.Level) {
		err = errors.Join(err, h.next.Handle(ctx, r.Clone()))
	}
	return err
}
func (h *destinationHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	primaryAttrs, nextAttrs := slices.Clone(attrs), slices.Clone(attrs)
	return &destinationHandler{stream: h.stream, primary: h.primary.WithAttrs(primaryAttrs), next: h.next.WithAttrs(nextAttrs)}
}
func (h *destinationHandler) WithGroup(name string) slog.Handler {
	return &destinationHandler{stream: h.stream, primary: h.primary.WithGroup(name), next: h.next.WithGroup(name)}
}

type handler struct {
	stream     *Stream
	json, next slog.Handler
}

func (h *handler) enabledStream(ctx context.Context, level slog.Level) bool {
	h.stream.mu.Lock()
	active := !h.stream.closed && len(h.stream.subs) > 0
	h.stream.mu.Unlock()
	return active && h.json.Enabled(ctx, level)
}
func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.enabledStream(ctx, level) || (h.next != nil && h.next.Enabled(ctx, level))
}
func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	var err error
	if h.enabledStream(ctx, r.Level) {
		err = h.json.Handle(ctx, r)
	}
	if h.next != nil && h.next.Enabled(ctx, r.Level) {
		err = errors.Join(err, h.next.Handle(ctx, r))
	}
	return err
}
func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	jsonAttrs, nextAttrs := slices.Clone(attrs), slices.Clone(attrs)
	n := &handler{stream: h.stream, json: h.json.WithAttrs(jsonAttrs), next: h.next}
	if n.next != nil {
		n.next = n.next.WithAttrs(nextAttrs)
	}
	return n
}
func (h *handler) WithGroup(name string) slog.Handler {
	n := &handler{stream: h.stream, json: h.json.WithGroup(name), next: h.next}
	if n.next != nil {
		n.next = n.next.WithGroup(name)
	}
	return n
}

type recordWriter struct{ stream *Stream }

func (w recordWriter) Write(raw []byte) (int, error) { return w.stream.write(raw) }

// write receives complete JSON records from the private slog JSON handler.
func (s *Stream) write(raw []byte) (int, error) {
	var wire struct {
		Time    time.Time                  `json:"time"`
		Level   string                     `json:"level"`
		Message string                     `json:"msg"`
		Attrs   map[string]json.RawMessage `json:"attrs"`
		Source  *Source                    `json:"source"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(s.subs) == 0 {
		return len(raw), nil
	}
	s.seq++
	if wire.Time.IsZero() {
		wire.Time = time.Now().UTC()
	}
	if wire.Attrs == nil {
		wire.Attrs = map[string]json.RawMessage{}
	}
	r := Record{Seq: s.seq, Time: wire.Time, Level: wire.Level, Message: wire.Message, Attrs: wire.Attrs, Source: wire.Source}
	size, fit := bound(&r, s.config.MaxRecordBytes)
	for sub := range s.subs {
		sub.enqueue(r, size, fit)
	}
	return len(raw), nil
}

func bound(r *Record, limit int) (int, bool) {
	size := func() int { b, _ := json.Marshal(r); return len(b) }
	if n := size(); n <= limit {
		return n, true
	}
	r.Truncated = true
	r.Attrs = map[string]json.RawMessage{}
	if n := size(); n <= limit {
		return n, true
	}
	r.Source = nil
	if n := size(); n <= limit {
		return n, true
	}
	message := r.Message
	r.Message = ""
	if size() > limit {
		return 0, false
	}
	lo, hi := 0, len(message)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		r.Message = validPrefix(message, mid)
		if size() <= limit {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	r.Message = strings.Clone(validPrefix(message, lo))
	return size(), true
}
func validPrefix(s string, n int) string {
	for n > 0 && !utf8.ValidString(s[:n]) {
		n--
	}
	return s[:n]
}
func cloneRecord(r Record) *Record {
	c := r
	c.Attrs = make(map[string]json.RawMessage, len(r.Attrs))
	for k, v := range r.Attrs {
		c.Attrs[k] = append(json.RawMessage(nil), v...)
	}
	if r.Source != nil {
		source := *r.Source
		c.Source = &source
	}
	return &c
}

type queued struct {
	record *Record
	bytes  int64
}

// Subscription drains through C until Close. Slow readers lose the oldest
// queued records and receive an explicit gap before later records.
type Subscription struct {
	C      <-chan Notification
	out    chan Notification
	stream *Stream
	mu     sync.Mutex
	config Config
	queue  []queued
	bytes  int64
	gap    *Gap
	wake   chan struct{}
	stop   chan struct{}
	done   chan struct{}
	closed bool
}

func (s *Stream) Subscribe() *Subscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(chan Notification)
	sub := &Subscription{C: out, out: out, stream: s, config: s.config, wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	if s.closed {
		sub.closed = true
		close(sub.stop)
	} else {
		s.subs[sub] = struct{}{}
	}
	go sub.run()
	return sub
}
func (s *Stream) Close() {
	s.mu.Lock()
	s.closed = true
	subs := make([]*Subscription, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.mu.Unlock()
	for _, sub := range subs {
		sub.Close()
	}
}
func (s *Subscription) Close() {
	s.stream.mu.Lock()
	delete(s.stream.subs, s)
	s.stream.mu.Unlock()
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.queue = nil
		s.bytes = 0
		s.gap = nil
		close(s.stop)
	}
	s.mu.Unlock()
	<-s.done
}
func (s *Subscription) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Subscription) drop(seq uint64) {
	if s.gap == nil {
		s.gap = &Gap{FromSeq: seq}
	}
	s.gap.ToSeq = seq
	s.gap.Count++
}
func (s *Subscription) evict() {
	q := s.queue[0]
	s.drop(q.record.Seq)
	s.bytes -= q.bytes
	s.queue[0] = queued{}
	s.queue = s.queue[1:]
	if len(s.queue) == 0 {
		s.queue = nil
	}
}
func (s *Subscription) configure(config Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config = config
	oversize := false
	for _, q := range s.queue {
		if q.bytes > int64(config.MaxRecordBytes) {
			oversize = true
			break
		}
	}
	for len(s.queue) > 0 && (oversize || len(s.queue) > config.MaxQueueRecords || s.bytes > config.MaxQueueBytes) {
		s.evict()
	}
	if cap(s.queue) > 2*config.MaxQueueRecords {
		s.queue = append([]queued(nil), s.queue...)
	}
	s.notify()
}
func (s *Subscription) enqueue(r Record, size int, fit bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	for len(s.queue) > 0 && (!fit || int64(size) > s.config.MaxQueueBytes || len(s.queue) >= s.config.MaxQueueRecords || s.bytes+int64(size) > s.config.MaxQueueBytes) {
		s.evict()
	}
	if !fit || int64(size) > s.config.MaxQueueBytes {
		s.drop(r.Seq)
	} else {
		s.queue = append(s.queue, queued{record: cloneRecord(r), bytes: int64(size)})
		s.bytes += int64(size)
	}
	s.notify()
}
func (s *Subscription) pop() (Notification, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Notification{}, false
	}
	if s.gap != nil {
		g := s.gap
		s.gap = nil
		return Notification{Dropped: g}, true
	}
	if len(s.queue) == 0 {
		return Notification{}, false
	}
	q := s.queue[0]
	s.queue[0] = queued{}
	s.queue = s.queue[1:]
	if len(s.queue) == 0 {
		s.queue = nil
	}
	s.bytes -= q.bytes
	return Notification{Record: q.record}, true
}
func (s *Subscription) run() {
	defer close(s.done)
	defer close(s.out)
	for {
		if n, ok := s.pop(); ok {
			select {
			case s.out <- n:
			case <-s.stop:
				return
			}
			continue
		}
		select {
		case <-s.wake:
		case <-s.stop:
			return
		}
	}
}
