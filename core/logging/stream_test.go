package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func receive(t *testing.T, sub *Subscription) Notification {
	t.Helper()
	select {
	case n, ok := <-sub.C:
		if !ok {
			t.Fatal("subscription closed")
		}
		return n
	case <-time.After(2 * time.Second):
		t.Fatal("log delivery timed out")
		return Notification{}
	}
}

func TestSlogGroupsSourceAndIndependentSubscribers(t *testing.T) {
	stream := New(nil, nil, Config{})
	defer stream.Close()
	a, b := stream.Subscribe(), stream.Subscribe()
	defer a.Close()
	defer b.Close()
	logger := slog.New(stream).With("sessionId", "ses_1").WithGroup("request").With("method", "GET").WithGroup("details")
	logger.Info("hello", "count", 7, "message", "attribute cannot replace envelope")
	x, y := receive(t, a).Record, receive(t, b).Record
	if x == nil || y == nil || x.Message != "hello" || x.Level != "INFO" || x.Time.IsZero() || x.Seq != 1 {
		t.Fatalf("record: %+v", x)
	}
	if x.Source == nil || !strings.HasSuffix(x.Source.File, "stream_test.go") || x.Source.Line <= 0 {
		t.Fatalf("source: %+v", x.Source)
	}
	var request struct {
		Method  string `json:"method"`
		Details struct {
			Count   int    `json:"count"`
			Message string `json:"message"`
		} `json:"details"`
	}
	if err := json.Unmarshal(x.Attrs["request"], &request); err != nil {
		t.Fatal(err)
	}
	if request.Method != "GET" || request.Details.Count != 7 || request.Details.Message != "attribute cannot replace envelope" {
		t.Fatalf("groups: %+v", request)
	}
	x.Attrs["sessionId"][1] = 'X'
	if string(y.Attrs["sessionId"]) != `"ses_1"` {
		t.Fatal("subscribers share mutable attributes")
	}
}

func TestStreamAndNextHandlerHaveIndependentLevelPolicies(t *testing.T) {
	level := new(slog.LevelVar)
	level.Set(slog.LevelWarn)
	var sink bytes.Buffer
	stream := New(level, slog.NewJSONHandler(&sink, &slog.HandlerOptions{Level: slog.LevelDebug}), Config{})
	defer stream.Close()
	sub := stream.Subscribe()
	defer sub.Close()
	logger := slog.New(stream)
	logger.Debug("next only")
	if !strings.Contains(sink.String(), "next only") {
		t.Fatal("next handler lost permitted log")
	}
	level.Set(slog.LevelDebug)
	logger.Debug("both")
	if n := receive(t, sub); n.Record == nil || n.Record.Message != "both" {
		t.Fatalf("stream level: %+v", n)
	}
	stream.Close()
	logger.Info("after close")
	if !strings.Contains(sink.String(), "after close") {
		t.Fatal("closing stream disabled next handler")
	}
}

func TestSlowSubscriberBoundsAndOrderedDropMarkers(t *testing.T) {
	stream := New(nil, nil, Config{MaxQueueRecords: 2, MaxQueueBytes: 1024, MaxRecordBytes: 512})
	defer stream.Close()
	sub := stream.Subscribe()
	defer sub.Close()
	logger := slog.New(stream)
	for i := 0; i < 100; i++ {
		logger.Info("bounded", "number", i)
	}
	sub.mu.Lock()
	queued, bytes := len(sub.queue), sub.bytes
	sub.mu.Unlock()
	if queued > 2 || bytes > 1024 {
		t.Fatalf("queue escaped bounds: %d %d", queued, bytes)
	}
	var last, total uint64
	for last < 100 {
		n := receive(t, sub)
		if n.Dropped != nil {
			g := n.Dropped
			if g.FromSeq != last+1 || g.Count != g.ToSeq-g.FromSeq+1 {
				t.Fatalf("out-of-order gap after %d: %+v", last, g)
			}
			last = g.ToSeq
			total += g.Count
		} else {
			if n.Record.Seq != last+1 {
				t.Fatalf("missing marker after %d: %+v", last, n.Record)
			}
			last = n.Record.Seq
			total++
		}
	}
	if total != 100 {
		t.Fatalf("records accounted: %d", total)
	}
}

func TestOversizeAndTinyLimits(t *testing.T) {
	stream := New(nil, nil, Config{MaxRecordBytes: 180})
	defer stream.Close()
	sub := stream.Subscribe()
	defer sub.Close()
	logger := slog.New(stream)
	logger.Info(strings.Repeat("界", 2000), "large", strings.Repeat("x", 4000))
	n := receive(t, sub)
	if n.Record == nil || !n.Record.Truncated {
		t.Fatalf("oversize record: %+v", n)
	}
	raw, err := json.Marshal(n.Record)
	if err != nil || len(raw) > 180 {
		t.Fatalf("record bytes=%d err=%v", len(raw), err)
	}
	stream.Configure(Config{MaxQueueRecords: 1, MaxQueueBytes: 1, MaxRecordBytes: 1})
	logger.Info("cannot fit envelope")
	n = receive(t, sub)
	if n.Record != nil || n.Dropped == nil || n.Dropped.Count != 1 {
		t.Fatalf("tiny limit: %+v", n)
	}
}

func TestConcurrentSubscribeConfigurePublishAndClose(t *testing.T) {
	stream := New(nil, nil, Config{})
	logger := slog.New(stream)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				sub := stream.Subscribe()
				logger.WithGroup("group").Info("concurrent", "nested", slog.GroupValue(slog.String("value", "ok")))
				stream.Configure(Config{MaxQueueRecords: 3, MaxQueueBytes: 800, MaxRecordBytes: 400})
				sub.Close()
			}
		}()
	}
	wg.Wait()
	stream.Close()
	sub := stream.Subscribe()
	defer sub.Close()
	if _, ok := <-sub.C; ok {
		t.Fatal("closed stream accepted subscription")
	}
}

func TestWithDestinationPreservesOutputsGroupsAndLevels(t *testing.T) {
	var original, extra bytes.Buffer
	stream := New(nil, slog.NewJSONHandler(&original, nil), Config{})
	defer stream.Close()
	sub := stream.Subscribe()
	defer sub.Close()
	added := slog.NewJSONHandler(&extra, &slog.HandlerOptions{Level: slog.LevelWarn})
	logger := slog.New(stream.WithDestination(added)).With("application", "demo").WithGroup("request")
	logger.Info("original and stream", "value", 1)
	logger.Warn("all destinations", "value", 2)
	for _, message := range []string{"original and stream", "all destinations"} {
		n := receive(t, sub)
		if n.Record == nil || n.Record.Message != message || string(n.Record.Attrs["application"]) != `"demo"` {
			t.Fatalf("record: %+v", n)
		}
		if !bytes.Contains(n.Record.Attrs["request"], []byte(`"value":`)) {
			t.Fatalf("group: %+v", n.Record.Attrs)
		}
	}
	if strings.Count(original.String(), "\n") != 2 || strings.Count(extra.String(), "\n") != 1 || !strings.Contains(extra.String(), `"request":{"value":2}`) {
		t.Fatalf("destinations: %s / %s", original.String(), extra.String())
	}
}

func TestWithDestinationDoesNotDuplicateSameStreamHandlers(t *testing.T) {
	stream := New(nil, nil, Config{})
	defer stream.Close()
	sub := stream.Subscribe()
	defer sub.Close()
	var sink bytes.Buffer
	derived := slog.New(stream).With("component", "host").WithGroup("app").Handler()
	combined := slog.New(stream.WithDestination(slog.NewJSONHandler(&sink, nil))).With("component", "host").WithGroup("app").Handler()
	for _, next := range []slog.Handler{stream, derived, combined} {
		composed := stream.WithDestination(next)
		if composed != next {
			t.Fatal("existing stream handler was wrapped again")
		}
		slog.New(composed).Info("once", "value", 7)
		n := receive(t, sub)
		stream.mu.Lock()
		latest := stream.seq
		stream.mu.Unlock()
		if n.Record == nil || n.Record.Seq != latest {
			t.Fatal("duplicate stream emission")
		}
		if next != stream && string(n.Record.Attrs["component"]) != `"host"` {
			t.Fatal("derived handler attributes lost")
		}
	}
}

type failingHandler struct{ err error }

func (h failingHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (h failingHandler) Handle(context.Context, slog.Record) error { return h.err }
func (h failingHandler) WithAttrs([]slog.Attr) slog.Handler        { return h }
func (h failingHandler) WithGroup(string) slog.Handler             { return h }

func TestWithDestinationJoinsHandlerErrors(t *testing.T) {
	a, b := errors.New("first"), errors.New("second")
	stream := New(nil, failingHandler{a}, Config{})
	defer stream.Close()
	err := stream.WithDestination(failingHandler{b}).Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "record", 0))
	if !errors.Is(err, a) || !errors.Is(err, b) {
		t.Fatalf("joined errors: %v", err)
	}
}

type mutatingAttrsHandler struct{}

func (mutatingAttrsHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (mutatingAttrsHandler) Handle(context.Context, slog.Record) error { return nil }
func (h mutatingAttrsHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) > 0 {
		attrs[0] = slog.String("overwritten", "mutated")
	}
	return h
}
func (h mutatingAttrsHandler) WithGroup(string) slog.Handler { return h }

func TestWithAttrsOwnershipIsIndependentAcrossDestinations(t *testing.T) {
	var sink bytes.Buffer
	stream := New(nil, mutatingAttrsHandler{}, Config{})
	defer stream.Close()
	sub := stream.Subscribe()
	defer sub.Close()
	logger := slog.New(stream.WithDestination(slog.NewJSONHandler(&sink, nil))).With("original", "value")
	logger.Info("test")
	if !strings.Contains(sink.String(), `"original":"value"`) || strings.Contains(sink.String(), "overwritten") {
		t.Fatalf("attributes leaked between destinations: %s", sink.String())
	}
	if n := receive(t, sub); n.Record == nil || string(n.Record.Attrs["original"]) != `"value"` {
		t.Fatalf("stream attrs: %+v", n)
	}
}
