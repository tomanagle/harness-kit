package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tomanagle/harness-kit/core/session"
)

func TestCLIBudgetSettingsParity(t *testing.T) {
	for _, rpc := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "rpc"}[rpc], func(t *testing.T) {
			flags := []string{"--store", t.TempDir(), "--runtimes", "fake", "--json"}
			if rpc {
				flags = append(flags, "--rpc")
			}
			call := func(args ...string) (string, string, int) { return run(t, "", append(args, flags...)...) }
			out, stderr, code := call("prompt", "--runtime", "fake", "--session-settings", `{"budget":{"maxTurnTokens":0}}`, "--turn-settings", `{"budget":{"maxTurnTokens":null}}`, "hello")
			var first turnResult
			if code != 0 || json.Unmarshal([]byte(out), &first) != nil {
				t.Fatal(out, stderr, code)
			}
			if _, stderr, code = call("prompt", "--resume", first.SessionID, "blocked"); code == 0 || !strings.Contains(stderr, "budget") {
				t.Fatal(stderr, code)
			}
			if _, stderr, code = call("sessions", "settings", first.SessionID, `{"budget":{"maxTurnTokens":null}}`); code != 0 {
				t.Fatal(stderr, code)
			}
			if _, stderr, code = call("prompt", "--resume", first.SessionID, "again"); code != 0 {
				t.Fatal(stderr, code)
			}
			out, stderr, code = call("sessions", "show", first.SessionID)
			var info session.Info
			if code != 0 || json.Unmarshal([]byte(out), &info) != nil || info.Usage.TotalTokens != first.Usage.TotalTokens*2 {
				t.Fatal(info, out, stderr, code)
			}
		})
	}
}
