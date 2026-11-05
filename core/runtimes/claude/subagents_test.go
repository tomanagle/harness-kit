package claude

import (
	"strings"
	"testing"

	"github.com/tomanagle/harness-kit/core/event"
)

func TestSubagentForegroundAndNestedParent(t *testing.T) {
	rec := &recorder{}
	r := &turnReader{s: &Session{h: rec}, turnID: "turn"}
	r.read(strings.NewReader(`{"type":"assistant","parent_tool_use_id":"parent","message":{"content":[{"type":"tool_use","id":"call","name":"Agent","input":{"description":"Review","prompt":"Review code"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call","is_error":true,"content":"Failed"}]}}
`))
	got := rec.ofType(event.SubagentUpdated)
	if len(got) != 2 {
		t.Fatalf("updates: %+v", got)
	}
	start, end := got[0].Data.(event.SubagentUpdatedData), got[1].Data.(event.SubagentUpdatedData)
	if start.ParentID != "parent" || start.Status != event.SubagentRunning || end.Status != event.SubagentFailed || end.Error != "Failed" {
		t.Fatalf("lifecycle: %+v %+v", start, end)
	}
}

func TestBackgroundSubagentWaitsForNotificationExcludesShellAndStaleRun(t *testing.T) {
	rec := &recorder{}
	r := &turnReader{s: &Session{h: rec}, turnID: "turn"}
	r.read(strings.NewReader(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call","name":"Task","input":{"description":"Review","run_in_background":true}}]}}
{"type":"system","subtype":"task_started","task_id":"child","tool_use_id":"call","task_type":"local_agent","is_backgrounded":true,"run_id":"02"}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call","content":"Launched"}]}}
{"type":"system","subtype":"task_started","task_id":"bash","task_type":"local_bash"}
{"type":"system","subtype":"task_notification","task_id":"bash","status":"completed"}
{"type":"system","subtype":"task_notification","task_id":"child","run_id":"01","status":"failed"}
{"type":"system","subtype":"task_notification","task_id":"child","run_id":"02","status":"completed","summary":"All good"}
`))
	got := rec.ofType(event.SubagentUpdated)
	if len(got) != 3 {
		t.Fatalf("updates: %+v", got)
	}
	mid, end := got[1].Data.(event.SubagentUpdatedData), got[2].Data.(event.SubagentUpdatedData)
	if mid.ID != "child" || mid.CallID != "call" || mid.Status != event.SubagentRunning || end.Status != event.SubagentCompleted || end.Output != "All good" {
		t.Fatalf("lifecycle: %+v %+v", mid, end)
	}
}

func TestTaskStartBeforeToolBlockKeepsNativeIdentity(t *testing.T) {
	rec := &recorder{}
	r := &turnReader{s: &Session{h: rec}, turnID: "turn"}
	r.read(strings.NewReader(`{"type":"system","subtype":"task_started","task_id":"child","tool_use_id":"call","task_type":"local_agent","is_backgrounded":true}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call","name":"Agent","input":{"description":"Review"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call","content":"Launched"}]}}
{"type":"system","subtype":"task_notification","task_id":"child","status":"completed","summary":"Done"}
`))
	got := rec.ofType(event.SubagentUpdated)
	if len(got) != 3 {
		t.Fatalf("updates: %+v", got)
	}
	for _, e := range got {
		if e.Data.(event.SubagentUpdatedData).ID != "child" {
			t.Fatalf("identity changed: %+v", got)
		}
	}
}
