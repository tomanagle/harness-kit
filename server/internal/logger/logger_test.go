package logger

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestLoggerUsesInjectedSinkAndLevel(t *testing.T) {
	var output bytes.Buffer
	log, err := New(&output, Config{Level: slog.LevelWarn})
	if err != nil {
		t.Fatal(err)
	}
	log.Info("filtered")
	log.Error("request failed", "operation", "sync")
	var entry map[string]any
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["msg"] != "request failed" || entry["operation"] != "sync" || entry["level"] != "ERROR" {
		t.Fatal(entry)
	}
}
