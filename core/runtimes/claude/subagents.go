package claude

import (
	"encoding/json"

	"github.com/tomanagle/harness-kit/core/event"
)

func (t *turnReader) subagentStart(b contentBlock, parent string) {
	if t.subagents == nil {
		t.subagents = make(map[string]*event.SubagentUpdatedData)
	}
	var input struct {
		Description string `json:"description"`
		Prompt      string `json:"prompt"`
		Background  bool   `json:"run_in_background"`
	}
	_ = json.Unmarshal(b.Input, &input)
	d := t.subagents[b.ID]
	if d == nil {
		d = &event.SubagentUpdatedData{ID: b.ID, CallID: b.ID, Status: event.SubagentRunning}
	}
	if parent != "" {
		d.ParentID = parent
	}
	if input.Description != "" {
		d.Title = input.Description
	}
	if input.Prompt != "" {
		d.Prompt = input.Prompt
	}
	d.Background = d.Background || input.Background
	if d.Title == "" {
		d.Title = b.Name
	}
	t.subagents[b.ID] = d
	t.emit(event.SubagentUpdated, *d)
}

func (t *turnReader) subagentResult(callID string, result event.ToolCompletedData) {
	d := t.subagents[callID]
	if d == nil || (d.Status != event.SubagentRunning && d.Status != event.SubagentUnknown) {
		return
	}
	// A background Agent tool returns a launch acknowledgement, not its result.
	if d.Background && result.Status != event.ToolStatusFailed {
		return
	}
	d.Output = result.Output
	d.Error = result.Error
	d.Status = event.SubagentCompleted
	if result.Status == event.ToolStatusFailed {
		d.Status = event.SubagentFailed
	}
	t.emit(event.SubagentUpdated, *d)
}

// Task notifications also describe Bash/Monitor work; only identified agents
// become subtasks. See code.claude.com/docs/en/agent-sdk/typescript.
func (t *turnReader) subagentTask(m streamLine) {
	if m.Ambient {
		return
	}
	if t.subagents == nil {
		t.subagents = make(map[string]*event.SubagentUpdatedData)
	}
	d := t.subagents[m.TaskID]
	if d == nil && m.ToolUseID != "" {
		d = t.subagents[m.ToolUseID]
	}
	if d == nil {
		if m.Subtype != "task_started" || (m.TaskType != "local_agent" && m.TaskType != "remote_agent") || m.TaskID == "" {
			return
		}
		d = &event.SubagentUpdatedData{ID: m.TaskID, CallID: m.ToolUseID, Status: event.SubagentRunning}
	}
	if d.RunID != "" && m.RunID != "" && m.RunID < d.RunID {
		return
	}
	if m.RunID != "" {
		d.RunID = m.RunID
	}
	if m.TaskID != "" {
		d.ID = m.TaskID
		t.subagents[m.TaskID] = d
	}
	if m.ToolUseID != "" {
		d.CallID = m.ToolUseID
		t.subagents[m.ToolUseID] = d
	}
	if m.ParentTaskID != "" {
		d.ParentID = m.ParentTaskID
	}
	if m.Description != "" {
		d.Title = m.Description
	}
	switch m.Subtype {
	case "task_started":
		d.Status = event.SubagentRunning
		d.Background = m.Background
		d.Output = ""
		d.Error = ""
	case "task_notification":
		d.Status = claudeSubagentStatus(m.Status)
		d.Output = m.Summary
		if d.Status == event.SubagentFailed {
			d.Error = m.Summary
		}
	case "task_updated":
		if m.Patch.Status != "" {
			d.Status = claudeSubagentStatus(m.Patch.Status)
		}
		if m.Patch.Background != nil {
			d.Background = *m.Patch.Background
		}
		if m.Patch.Description != "" {
			d.Title = m.Patch.Description
		}
		if m.Patch.Error != "" {
			d.Error = m.Patch.Error
		}
	}
	t.emit(event.SubagentUpdated, *d)
}

func claudeSubagentStatus(status string) event.SubagentStatus {
	switch status {
	case "running", "pending":
		return event.SubagentRunning
	case "completed":
		return event.SubagentCompleted
	case "failed":
		return event.SubagentFailed
	case "stopped", "killed":
		return event.SubagentCancelled
	default:
		return event.SubagentUnknown
	}
}
