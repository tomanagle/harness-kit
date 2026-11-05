package codex

import (
	"sort"

	"github.com/tomanagle/harness-kit/core/event"
)

// Native collab tool schemas: openai/codex app-server-protocol v2.
func (s *Session) subagents(turnID string, it threadItem, completed bool) {
	spawn := it.Tool == "spawnAgent" || it.Tool == "spawn_agent"
	ids := append([]string(nil), it.ReceiverThreadIDs...)
	for id := range it.AgentsStates {
		found := false
		for _, existing := range ids {
			if existing == id {
				found = true
			}
		}
		if !found {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 && spawn {
		ids = []string{it.ID}
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		d := event.SubagentUpdatedData{ID: id, Status: event.SubagentUnknown}
		if spawn {
			d.CallID = it.ID
			d.Title = it.Prompt
			d.Prompt = it.Prompt
			if !completed {
				d.Status = event.SubagentRunning
			}
		}
		if state, ok := it.AgentsStates[id]; ok {
			d.Output = state.Message
			switch state.Status {
			case "pendingInit", "running":
				d.Status = event.SubagentRunning
			case "completed":
				d.Status = event.SubagentCompleted
			case "errored":
				d.Status = event.SubagentFailed
				d.Error = state.Message
			case "shutdown", "interrupted":
				d.Status = event.SubagentCancelled
			}
		} else if spawn && completed && it.Status == "failed" {
			d.Status = event.SubagentFailed
			if it.Error != nil {
				d.Error = it.Error.Message
			}
		}
		s.mu.Lock()
		if s.childStates == nil {
			s.childStates = make(map[string]event.SubagentStatus)
		}
		previous := s.childStates[id]
		// Closing a finished worker releases it; it does not cancel its work.
		if it.AgentsStates[id].Status == "shutdown" && (previous == event.SubagentCompleted || previous == event.SubagentFailed) {
			d.Status = previous
		}
		s.childStates[id] = d.Status
		s.mu.Unlock()
		s.emit(turnID, event.SubagentUpdated, d)
	}
}
