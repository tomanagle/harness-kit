package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/session"
)

func TestCLIConfiguration(t *testing.T) {
	for _, mode := range [][]string{nil, {"-rpc"}} {
		t.Run(strings.Join(mode, ""), func(t *testing.T) {
			store := t.TempDir()
			args := append([]string{"prompt", "-runtime", "fake", "--store", store, "-model", "base", "-effort", "low", "-turn-config", `{"model":"temporary","effort":"high"}`, "--output", "stream-json", "hello"}, mode...)
			out, stderr, code := run(t, "", args...)
			if code != 0 {
				t.Fatalf("prompt: %d %s %s", code, out, stderr)
			}
			var id string
			var found bool
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				var ev event.Event
				if err := json.Unmarshal([]byte(line), &ev); err != nil {
					t.Fatal(err, line)
				}
				id = ev.SessionID
				if d, ok := ev.Data.(event.TurnStartedData); ok {
					found = true
					if d.Config == nil || d.Config.Model != "temporary" || d.Config.Effort != "high" {
						t.Fatalf("turn snapshot: %+v", d.Config)
					}
				}
			}
			if !found || id == "" {
				t.Fatalf("missing turn: %s", out)
			}
			args = append([]string{"sessions", "show", id, "--store", store, "--json"}, mode...)
			out, stderr, code = run(t, "", args...)
			var info session.Info
			if code != 0 || json.Unmarshal([]byte(out), &info) != nil || info.Config == nil || info.Config.Model != "base" || info.Config.Effort != "low" {
				t.Fatalf("one-off changed defaults: %d %s %s", code, out, stderr)
			}
			args = append([]string{"sessions", "configure", id, `{"model":"configured","effort":"medium"}`, "--store", store, "--runtimes", "fake", "--json"}, mode...)
			out, stderr, code = run(t, "", args...)
			if code != 0 || json.Unmarshal([]byte(out), &info) != nil || info.Config.Model != "configured" {
				t.Fatalf("configure: %d %s %s", code, out, stderr)
			}
			args = append([]string{"prompt", "-resume", id, "--store", store, "-effort", "high", "--output", "stream-json", "again"}, mode...)
			out, stderr, code = run(t, "", args...)
			if code != 0 || !strings.Contains(out, `"effort":"high"`) || !strings.Contains(out, `"model":"configured"`) {
				t.Fatalf("resume config: %d %s %s", code, out, stderr)
			}
		})
	}
}

func TestCLIChatConfigure(t *testing.T) {
	out, stderr, code := run(t, "/configure {\"model\":\"changed\",\"effort\":\"high\"}\nhello\n/quit\n", "chat", "-runtime", "fake", "--store", "memory", "--output", "stream-json")
	if code != 0 || !strings.Contains(out, `"model":"changed"`) || !strings.Contains(out, `"effort":"high"`) || !strings.Contains(stderr, "session defaults updated") {
		t.Fatalf("chat configure: %d %s %s", code, out, stderr)
	}
	for _, raw := range []string{"null", "[]", `{"model":null}`, `{"effort":null}`, `{"unknown":true}`} {
		if _, err := parseConfigPatch(raw); err == nil {
			t.Fatalf("accepted malformed patch %s", raw)
		}
	}
}
