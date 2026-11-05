// Package integration_test exercises real runtime lifecycle behavior through the
// same persisted session manager used by sidecar clients. It never runs in CI by
// default and never reads a CLI's credential files.
package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/mcp"
	"github.com/tomanagle/harness-kit/core/runtimes/claude"
	"github.com/tomanagle/harness-kit/core/runtimes/codex"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
)

// TestLiveLifecycle uses four short completed prompts and one interrupted prompt
// per runtime. Models remain caller-selectable; distinct model override coverage
// requires HARNESS_KIT_LIVE_<RUNTIME>_OVERRIDE_MODEL. Native transcripts remain in
// the CLI's own session store, while all kit data/workspaces use temporary dirs.
func TestLiveLifecycle(t *testing.T) {
	if os.Getenv("HARNESS_KIT_LIVE") != "1" || os.Getenv("HARNESS_KIT_LIVE_LIFECYCLE") != "1" {
		t.Skip("set HARNESS_KIT_LIVE=1 and HARNESS_KIT_LIVE_LIFECYCLE=1; consumes signed-in CLI quota")
	}
	for _, name := range []string{"codex", "claude"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			cwd := t.TempDir()
			no := false
			cfg := agent.SessionConfig{Cwd: cwd, Model: os.Getenv("HARNESS_KIT_LIVE_" + strings.ToUpper(name) + "_MODEL"), Effort: "low", DisableHostTools: true, MCP: &mcp.Config{InheritNative: &no}, Instructions: "This is a lifecycle integration test. Never invoke tools, read files, delegate, or modify anything. Reply briefly to the prompt."}
			var rt agent.Runtime
			if name == "claude" {
				rt = claude.New()
				empty := []string{}
				cfg.Options = claude.Options{Tools: &empty, PermissionMode: "dontAsk", MaxTurns: 2, MaxBudgetUSD: .50}
				if cfg.Model == "" {
					cfg.Model = "haiku"
				}
			} else {
				rt = codex.New()
				cfg.Options = codex.Options{Sandbox: "read-only", ApprovalPolicy: "never"}
			}
			discovery, err := rt.Discover(ctx)
			if err != nil || !discovery.Ready || discovery.Auth.State != agent.AuthAuthenticated {
				t.Skipf("%s unavailable or not signed in: version=%s auth=%s problems=%v err=%v", name, discovery.Version, discovery.Auth.State, discovery.Problems, err)
			}
			t.Logf("runtime=%s version=%s auth=%s", name, discovery.Version, discovery.Auth.Method)
			tracked := map[int]bool{}
			storeDir := t.TempDir()
			newManager := func() *session.Manager {
				store, err := session.NewFileStore(storeDir)
				if err != nil {
					t.Fatal(err)
				}
				manager := session.NewManager(agent.NewRegistry(rt), session.Options{Store: store, EventLogSize: 2})
				t.Cleanup(func() {
					cleanupCtx, c := context.WithTimeout(context.Background(), 20*time.Second)
					defer c()
					if err := manager.Shutdown(cleanupCtx); err != nil {
						t.Errorf("shutdown: %v", err)
					}
				})
				return manager
			}
			m := newManager()
			info, err := m.Create(ctx, session.CreateParams{Runtime: name, Config: cfg, Title: "Harness Kit live lifecycle test"})
			if err != nil {
				t.Fatal(err)
			}
			captureChildren(t, tracked)
			first := agent.Text("Remember PINEAPPLE as the branch word. Reply only PINEAPPLE.")
			if name == "claude" {
				imagePath := filepath.Join(cwd, "red.png")
				writeRedImage(t, imagePath)
				first = append(agent.Text("Remember PINEAPPLE as the branch word. Inspect the attached image and reply only PINEAPPLE followed by its color."), agent.InputPart{Type: "image", Path: imagePath})
			}
			initial := prompt(t, ctx, m, info.ID, "initial", first, nil)
			expectText(t, initial, "PINEAPPLE")
			if name == "claude" {
				expectText(t, initial, "RED")
			}
			info, err = m.Get(ctx, info.ID)
			if err != nil || info.RuntimeSessionID == "" {
				t.Fatalf("native session missing: %+v %v", info, err)
			}
			nativeParent := info.RuntimeSessionID
			if err = m.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			before := events(t, m, info.ID)
			m = newManager()
			replayed := events(t, m, info.ID)
			if !reflect.DeepEqual(before, replayed) {
				t.Fatal("persisted transcript changed across manager restart")
			}
			if len(replayed) < 5 {
				t.Fatal("expected prompt, streamed reply and lifecycle events on disk")
			}
			if _, err = m.Restore(ctx, info.ID); err != nil {
				t.Fatal(err)
			}
			captureChildren(t, tracked)
			overrideModel := os.Getenv("HARNESS_KIT_LIVE_" + strings.ToUpper(name) + "_OVERRIDE_MODEL")
			selection := map[string]any{"effort": "medium"}
			if overrideModel != "" {
				selection["model"] = overrideModel
			} else {
				t.Log("distinct model override not selected; set HARNESS_KIT_LIVE_" + strings.ToUpper(name) + "_OVERRIDE_MODEL for that coverage")
			}
			raw, _ := json.Marshal(selection)
			resumed := prompt(t, ctx, m, info.ID, "override", agent.Text("What branch word did I ask you to remember? Reply only that word."), settings.Patch{"session": raw})
			expectText(t, resumed, "PINEAPPLE")
			selected := turnConfig(t, resumed)
			if selected.Effort != "medium" || (overrideModel != "" && selected.Model != overrideModel) {
				t.Fatalf("override not accepted: %+v", selected)
			}
			if name == "claude" && overrideModel != "" {
				if observed := readyModel(resumed); !strings.Contains(observed, overrideModel) {
					t.Fatalf("native model=%q override=%q", observed, overrideModel)
				}
			}
			current, err := m.Get(ctx, info.ID)
			if err != nil || current.RuntimeSessionID != nativeParent {
				t.Fatalf("resume changed native identity: %+v %v", current, err)
			}
			parentBeforeFork := events(t, m, info.ID)
			branch, err := m.Fork(ctx, info.ID, session.ForkParams{Title: "Harness Kit live child"})
			if err != nil {
				t.Fatal(err)
			}
			captureChildren(t, tracked)
			child := prompt(t, ctx, m, branch.ID, "child", agent.Text("First reply only with the current branch word. After replying, remember PEACH as the branch word from now on."), nil)
			expectText(t, child, "PINEAPPLE")
			branch, err = m.Get(ctx, branch.ID)
			if err != nil || branch.RuntimeSessionID == "" || branch.RuntimeSessionID == nativeParent || branch.ForkPending {
				t.Fatalf("native fork failed: %+v %v", branch, err)
			}
			if !reflect.DeepEqual(parentBeforeFork, events(t, m, info.ID)) {
				t.Fatal("child turn changed parent's persisted transcript")
			}
			inherited := prompt(t, ctx, m, info.ID, "inherited", agent.Text("What is the branch word now? Reply only that word."), nil)
			expectText(t, inherited, "PINEAPPLE")
			selectionAfter := turnConfig(t, inherited)
			if selectionAfter.Effort != "low" || cfg.Model != "" && selectionAfter.Model != cfg.Model {
				t.Fatalf("one-turn override leaked: %+v base=%+v", selectionAfter, cfg)
			}
			t.Logf("settings: inherited model=%s effort=%s; override model=%s effort=%s", selectionAfter.Model, selectionAfter.Effort, selected.Model, selected.Effort)
			if name == "claude" {
				if readyModel(inherited) != readyModel(initial) {
					t.Fatalf("native model override leaked: initial=%s inherited=%s", readyModel(initial), readyModel(inherited))
				}
				firstCost, secondCost, thirdCost := sessionCost(initial), sessionCost(resumed), sessionCost(inherited)
				if firstCost <= 0 || secondCost <= firstCost || thirdCost <= secondCost {
					t.Fatalf("expected restored cumulative cost on Claude %s: initial=%g resumed=%g inherited=%g", discovery.Version, firstCost, secondCost, thirdCost)
				}
				t.Logf("native cumulative total_cost_usd: initial=%.8f resumed=%.8f inherited=%.8f; fork=%.8f", firstCost, secondCost, thirdCost, sessionCost(child))
			}
			// Cancel only once the native process has emitted an observable event.
			sub := m.Subscribe(event.ForSession(info.ID), 64)
			defer sub.Close()
			turn, err := m.Prompt(ctx, info.ID, session.PromptParams{TurnID: "cancel", Input: agent.Text("Count from 1 to 10000, one number per line, without tools.")})
			if err != nil {
				t.Fatal(err)
			}
			for {
				select {
				case ev := <-sub.C:
					if ev.TurnID == turn.ID && (ev.Type == event.MessageDelta || ev.Type == event.RuntimeStatus) {
						captureChildren(t, tracked)
						if err = m.Cancel(info.ID); err != nil {
							t.Fatal(err)
						}
						goto cancelled
					}
				case <-turn.Done():
					t.Fatal("turn completed before cancellation could be exercised")
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
		cancelled:
			result, err := turn.Wait(ctx)
			if err != nil || result.Status != event.TurnStatusCancelled {
				t.Fatalf("cancel: %+v %v", result, err)
			}
			if err = m.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			assertChildrenExited(t, tracked)
			t.Logf("persisted events=%d, independent fork, settings inheritance, cancellation and process cleanup passed", len(events(t, m, info.ID)))
		})
	}
}

func prompt(t *testing.T, ctx context.Context, m *session.Manager, id, turnID string, input []agent.InputPart, patch settings.Patch) []event.Event {
	t.Helper()
	turn, err := m.Prompt(ctx, id, session.PromptParams{TurnID: turnID, Input: input, Settings: patch})
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Wait(ctx)
	if err != nil || result.Status != event.TurnStatusCompleted {
		t.Fatalf("turn %s: %+v %v", turnID, result, err)
	}
	var selected []event.Event
	for _, ev := range events(t, m, id) {
		if ev.TurnID == turnID {
			selected = append(selected, ev)
		}
	}
	return selected
}
func events(t *testing.T, m *session.Manager, id string) []event.Event {
	t.Helper()
	evs, truncated, err := m.Events(id, 0, 0)
	if err != nil || truncated {
		t.Fatalf("events: truncated=%v err=%v", truncated, err)
	}
	return evs
}
func expectText(t *testing.T, evs []event.Event, want string) {
	t.Helper()
	var text strings.Builder
	for _, ev := range evs {
		if d, ok := ev.Data.(event.MessageDeltaData); ok {
			text.WriteString(d.Text)
		}
	}
	if !strings.Contains(strings.ToUpper(text.String()), want) {
		t.Fatalf("reply %q missing %s", text.String(), want)
	}
}
func turnConfig(t *testing.T, evs []event.Event) agent.TurnConfig {
	t.Helper()
	for _, ev := range evs {
		if d, ok := ev.Data.(event.TurnStartedData); ok && d.Config != nil {
			return *d.Config
		}
	}
	t.Fatal("missing effective turn settings")
	return agent.TurnConfig{}
}
func readyModel(evs []event.Event) string {
	for _, ev := range evs {
		if d, ok := ev.Data.(event.RuntimeStatusData); ok && d.Status == "ready" {
			var info struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(d.Detail, &info)
			if info.Model != "" {
				return info.Model
			}
		}
	}
	return ""
}
func sessionCost(evs []event.Event) float64 {
	var cost float64
	for _, ev := range evs {
		if d, ok := ev.Data.(event.UsageData); ok && d.Scope == "session" && d.CostUSD != nil {
			cost = *d.CostUSD
		}
	}
	return cost
}
func writeRedImage(t *testing.T, path string) {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			im.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = png.Encode(f, im); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
}

// ps is used only to observe our test process's descendants; no arguments or
// credential-bearing environment are read. The check is available on macOS/Linux.
func processParents() (map[int]int, error) {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=").Output()
	if err != nil {
		return nil, err
	}
	pids := map[int]int{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, _ := strconv.Atoi(fields[0])
		parent, _ := strconv.Atoi(fields[1])
		pids[pid] = parent
	}
	return pids, nil
}
func captureChildren(t *testing.T, tracked map[int]bool) {
	t.Helper()
	parents, err := processParents()
	if err != nil {
		t.Logf("process-tree observation unavailable: %v", err)
		return
	}
	for pid, parent := range parents {
		for parent != 0 {
			if parent == os.Getpid() { // Ignore the already-reaped ps child itself.
				if _, stillAlive := parents[pid]; stillAlive {
					tracked[pid] = true
				}
				break
			}
			next, ok := parents[parent]
			if !ok || next == parent {
				break
			}
			parent = next
		}
	}
}
func assertChildrenExited(t *testing.T, tracked map[int]bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		parents, err := processParents()
		if err != nil {
			t.Logf("process cleanup observation unavailable: %v", err)
			return
		}
		var alive []int
		for pid := range tracked {
			if _, ok := parents[pid]; ok {
				alive = append(alive, pid)
			}
		}
		if len(alive) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(fmt.Sprintf("runtime descendants still running after shutdown: %v", alive))
		}
		time.Sleep(25 * time.Millisecond)
	}
}
