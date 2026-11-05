package cli

import (
	"encoding/json"
	"testing"

	"github.com/tomanagle/harness-kit/core/session"
)

func TestCLICatalogAndFork(t *testing.T) {
	for _, rpc := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "rpc"}[rpc], func(t *testing.T) {
			flags := []string{"--store", t.TempDir(), "--runtimes", "fake", "--json"}
			if rpc {
				flags = append(flags, "--rpc")
			}
			call := func(args ...string) string {
				t.Helper()
				out, stderr, code := run(t, "", append(args, flags...)...)
				if code != 0 {
					t.Fatalf("%v: %d %s %s", args, code, out, stderr)
				}
				return out
			}
			var result struct{ SessionID string }
			if err := json.Unmarshal([]byte(call("prompt", "--runtime", "fake", "inherited text")), &result); err != nil {
				t.Fatal(err)
			}
			call("sessions", "tags", result.SessionID, "work")
			call("sessions", "archive", result.SessionID)
			var list []session.Info
			if err := json.Unmarshal([]byte(call("sessions", "list", "--archived", "true", "--tags", "work", "--query", "inherited")), &list); err != nil || len(list) != 1 {
				t.Fatal(list, err)
			}
			parent := list[0]
			var child session.Info
			if err := json.Unmarshal([]byte(call("sessions", "fork", result.SessionID, "--title", "Branch", "--session-settings", `{"budget":{"maxTurnTokens":123}}`)), &child); err != nil {
				t.Fatal(err)
			}
			if child.RuntimeSessionID == parent.RuntimeSessionID || child.ForkedFrom != parent.ID || child.Archived || child.Title != "Branch" {
				t.Fatal(child)
			}
			if child.Budget.MaxTurnTokens == nil || *child.Budget.MaxTurnTokens != 123 || parent.Budget.MaxTurnTokens != nil {
				t.Fatalf("fork settings were not independent: child=%+v parent=%+v", child.Budget, parent.Budget)
			}
			call("sessions", "unarchive", result.SessionID)
			if err := json.Unmarshal([]byte(call("sessions", "list", "--archived", "true")), &list); err != nil || len(list) != 0 {
				t.Fatal(list, err)
			}
		})
	}
}
