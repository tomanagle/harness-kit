package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/workspace"
)

func TestCLIWorkspaceLifecycle(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	for _, rpc := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "rpc"}[rpc], func(t *testing.T) {
			repo := t.TempDir()
			gitRun := func(args ...string) {
				t.Helper()
				cmd := exec.Command(git, args...)
				cmd.Dir = repo
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %s %v", args, out, err)
				}
			}
			gitRun("init", "-q")
			gitRun("config", "core.autocrlf", "false")
			if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("base\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitRun("add", "file.txt")
			gitRun("-c", "user.name=Kit Test", "-c", "user.email=kit@example.test", "-c", "commit.gpgsign=false", "commit", "-qm", "base")
			flags := []string{"--store", t.TempDir(), "--runtimes", "fake", "--git-path", git, "--no-kit-mcp", "--json"}
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
			selection, err := json.Marshal(map[string]any{"workspace": map[string]any{"mode": "worktree", "repo": repo, "root": t.TempDir(), "captureDiff": true}})
			if err != nil {
				t.Fatal(err)
			}
			var turn turnResult
			if err := json.Unmarshal([]byte(call("prompt", "--runtime", "fake", "--cwd", repo, "--session-settings", string(selection), "hello")), &turn); err != nil {
				t.Fatal(err)
			}
			var info session.Info
			if err := json.Unmarshal([]byte(call("sessions", "show", turn.SessionID)), &info); err != nil || info.Workspace == nil || !info.Workspace.Owned {
				t.Fatalf("workspace: %+v %v", info.Workspace, err)
			}
			call("sessions", "remove-workspace", turn.SessionID)
			if _, err := os.Stat(info.Workspace.Path); !os.IsNotExist(err) {
				t.Fatalf("worktree still exists: %v", err)
			}
			var diff workspace.Diff
			if err := json.Unmarshal([]byte(call("sessions", "diff", turn.SessionID, turn.TurnID, "--max-bytes", "1024")), &diff); err != nil || !diff.Complete || diff.TurnID != turn.TurnID {
				t.Fatalf("saved diff after cleanup: %+v %v", diff, err)
			}
		})
	}
}
