package session_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/logging"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
)

func nextLog(t *testing.T, sub *logging.Subscription) logging.Record {
	t.Helper()
	select {
	case n, ok := <-sub.C:
		if !ok || n.Record == nil {
			t.Fatalf("expected log, got %+v (open %t)", n, ok)
		}
		return *n.Record
	case <-time.After(time.Second):
		t.Fatal("log was not streamed")
		return logging.Record{}
	}
}

func TestManagerLoggingUsesUnifiedLiveSettings(t *testing.T) {
	m := newManager(t, session.Options{})
	sub := m.Logs().Subscribe()
	defer sub.Close()
	if m.Logger().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug enabled by default")
	}
	snapshot, err := m.ConfigureSettings(context.Background(), settings.ConfigureParams{Patch: settings.Patch{"logging": json.RawMessage(`{"level":"debug","maxQueueRecords":4,"maxQueueBytes":2048,"maxRecordBytes":1024}`)}})
	if err != nil || !snapshot.Fields["logging.level"].Writable {
		t.Fatal(snapshot, err)
	}
	limits := m.Logs().Config()
	if limits.MaxQueueRecords != 4 || limits.MaxQueueBytes != 2048 || limits.MaxRecordBytes != 1024 {
		t.Fatal(limits)
	}
	m.Logger().With("sessionId", "example").WithGroup("app").Debug("visible", "count", 3)
	record := nextLog(t, sub)
	if record.Level != "DEBUG" || record.Message != "visible" || string(record.Attrs["sessionId"]) != `"example"` || string(record.Attrs["app"]) != `{"count":3}` {
		t.Fatal(record)
	}
	_, err = m.ConfigureSettings(context.Background(), settings.ConfigureParams{Patch: settings.Patch{"logging": json.RawMessage(`{"level":"error"}`)}})
	if err != nil || m.Logger().Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"maxQueueRecords":0}`, `{"maxQueueBytes":-1}`, `{"maxRecordBytes":0}`} {
		if _, err = m.ConfigureSettings(context.Background(), settings.ConfigureParams{Patch: settings.Patch{"logging": json.RawMessage(raw)}}); err == nil {
			t.Fatalf("invalid settings accepted: %s", raw)
		}
	}
}

func TestCoreLifecycleLogsOmitConversationContent(t *testing.T) {
	m := newManager(t, session.Options{})
	sub := m.Logs().Subscribe()
	defer sub.Close()
	info := create(t, m, agent.SessionConfig{})
	collect(t, m, info.ID, "private-prompt-content-should-not-be-logged")
	if err := m.Close(context.Background(), info.ID); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for !seen["session closed"] {
		r := nextLog(t, sub)
		seen[r.Message] = true
		encoded, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "private-prompt-content") {
			t.Fatal("conversation leaked into diagnostics")
		}
		if string(r.Attrs["sessionId"]) != `"`+info.ID+`"` {
			t.Fatal(r)
		}
	}
	if !seen["session started"] || !seen["turn completed"] {
		t.Fatal(seen)
	}
}

func TestManagerOwnsOnlyItsDefaultLogStream(t *testing.T) {
	ctx := context.Background()
	owned := newManager(t, session.Options{})
	sub := owned.Logs().Subscribe()
	if err := owned.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-sub.C:
		if ok {
			t.Fatal("owned stream still open")
		}
	case <-time.After(time.Second):
		t.Fatal("owned subscriber not released")
	}
	shared := logging.New(nil, nil, logging.Config{MaxQueueRecords: 7})
	defer shared.Close()
	m := newManager(t, session.Options{Logs: shared})
	snapshot, err := m.Settings(ctx, settings.Target{})
	if err != nil || snapshot.Values.Logging.MaxQueueRecords != 7 {
		t.Fatal(snapshot, err)
	}
	other := shared.Subscribe()
	defer other.Close()
	if err = m.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	slog.New(shared).Info("host still owns stream")
	if r := nextLog(t, other); r.Message != "host still owns stream" {
		t.Fatal(r)
	}
}

func TestWrappedStoreRetainsLocationCapability(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wrapped := struct{ *session.FileStore }{store}
	m := newManager(t, session.Options{Store: wrapped})
	snapshot, err := m.Settings(context.Background(), settings.Target{})
	if err != nil || snapshot.Values.Process.Store != store.Dir() {
		t.Fatal(snapshot.Values.Process.Store, err)
	}
}

func TestSharedStreamPreservesInjectedHostLogger(t *testing.T) {
	var output bytes.Buffer
	stream := logging.New(nil, nil, logging.Config{})
	defer stream.Close()
	m := newManager(t, session.Options{Logs: stream, Logger: slog.New(slog.NewJSONHandler(&output, nil))})
	sub := stream.Subscribe()
	defer sub.Close()
	m.Logger().Info("both destinations", "sessionId", "example")
	if nextLog(t, sub).Message != "both destinations" || !strings.Contains(output.String(), "both destinations") {
		t.Fatal(output.String())
	}
}
