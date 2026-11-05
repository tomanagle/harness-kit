package codex

import (
	"encoding/json"
	"testing"

	"github.com/tomanagle/harness-kit/core/event"
)

func TestCollabSpawnDoesNotCompleteChildAndWaitUpdatesSameChild(t *testing.T) {
	rec := &recorder{}
	s := &Session{h: rec}
	for _, raw := range []string{
		`{"type":"collabAgentToolCall","id":"spawn","tool":"spawnAgent","status":"completed","prompt":"Review code","receiverThreadIds":["child"],"agentsStates":{"child":{"status":"running"}}}`,
		`{"type":"collabAgentToolCall","id":"wait","tool":"wait","status":"completed","receiverThreadIds":["child"],"agentsStates":{"child":{"status":"completed","message":"Looks good"}}}`,
	} {
		var item threadItem
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			t.Fatal(err)
		}
		s.itemCompleted("turn", item)
	}
	got := rec.ofType(event.SubagentUpdated)
	if len(got) != 2 {
		t.Fatalf("updates: %+v", got)
	}
	start, end := got[0].Data.(event.SubagentUpdatedData), got[1].Data.(event.SubagentUpdatedData)
	if start.Status != event.SubagentRunning || start.CallID != "spawn" || start.Title != "Review code" || end.ID != start.ID || end.Status != event.SubagentCompleted || end.Output != "Looks good" || end.CallID != "" {
		t.Fatalf("bad lifecycle: %+v %+v", start, end)
	}
}

func TestCollabUnknownStatesAndFailedSpawn(t *testing.T) {
	rec := &recorder{}
	s := &Session{h: rec}
	s.itemCompleted("turn", threadItem{Type: "collabAgentToolCall", ID: "spawn", Tool: "spawnAgent", Status: "failed"})
	s.itemCompleted("turn", threadItem{Type: "collabAgentToolCall", ID: "wait", Tool: "wait", Status: "completed", ReceiverThreadIDs: []string{"missing"}})
	got := rec.ofType(event.SubagentUpdated)
	if got[0].Data.(event.SubagentUpdatedData).Status != event.SubagentFailed || got[1].Data.(event.SubagentUpdatedData).Status != event.SubagentUnknown {
		t.Fatalf("invented child completion: %+v", got)
	}
}

func TestClosingFinishedChildPreservesOutcome(t *testing.T) {
	rec := &recorder{}
	s := &Session{h: rec}
	for _, raw := range []string{
		`{"type":"collabAgentToolCall","id":"wait","tool":"wait","agentsStates":{"child":{"status":"completed","message":"Done"}}}`,
		`{"type":"collabAgentToolCall","id":"close","tool":"closeAgent","agentsStates":{"child":{"status":"shutdown"}}}`,
	} {
		var item threadItem
		_ = json.Unmarshal([]byte(raw), &item)
		s.itemCompleted("turn", item)
	}
	got := rec.ofType(event.SubagentUpdated)
	if got[1].Data.(event.SubagentUpdatedData).Status != event.SubagentCompleted {
		t.Fatalf("closing worker erased result: %+v", got)
	}
}
