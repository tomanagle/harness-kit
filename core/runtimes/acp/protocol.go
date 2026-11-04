package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/internal/jsonrpc"
	"github.com/tomanagle/harness-kit/core/permission"
)

type capabilities struct {
	LoadSession bool `json:"loadSession"`
	Prompt      struct {
		Image bool `json:"image"`
	} `json:"promptCapabilities"`
	MCP struct {
		HTTP bool `json:"http"`
		SSE  bool `json:"sse"`
	} `json:"mcpCapabilities"`
	Session map[string]json.RawMessage `json:"sessionCapabilities"`
}

func (c capabilities) session(name string) bool {
	value := strings.TrimSpace(string(c.Session[name]))
	return len(value) > 0 && value[0] == '{'
}

type initializeResponse struct {
	ProtocolVersion   int          `json:"protocolVersion"`
	AgentCapabilities capabilities `json:"agentCapabilities"`
	AgentInfo         ClientInfo   `json:"agentInfo"`
	AuthMethods       []struct {
		ID string `json:"id"`
	} `json:"authMethods"`
}
type sessionResponse struct {
	SessionID     string         `json:"sessionId"`
	ConfigOptions []ConfigOption `json:"configOptions"`
}
type promptResponse struct {
	StopReason string `json:"stopReason"`
}
type turnState struct {
	id                  string
	ctx                 context.Context
	cancel              context.CancelFunc
	itemID, kind, text  string
	itemSeq             int
	truncated           bool
	tools               map[string]*toolState
	compact             bool
	compactionConfirmed bool
	compactionCancelled bool
	compactionFailure   string
}
type toolState struct {
	name, title, kind, status, output string
	outputBytes                       int
	done                              bool
}
type toolUpdate struct {
	ID        string          `json:"toolCallId"`
	Name      *string         `json:"name"`
	Title     *string         `json:"title"`
	Kind      *string         `json:"kind"`
	Status    *string         `json:"status"`
	RawInput  json.RawMessage `json:"rawInput"`
	RawOutput json.RawMessage `json:"rawOutput"`
	Content   []struct {
		Type    string `json:"type"`
		Content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"content"`
	Locations []struct {
		Path string `json:"path"`
	} `json:"locations"`
}

// lockEvents serializes a bounded batch of state changes and their delivery.
// Never call a host while holding mu: a host may read session configuration or
// capability state while processing an event (or while another event waits).
func (s *Session) lockEvents() {
	s.eventMu.Lock()
	s.mu.Lock()
}

func (s *Session) unlockEvents() {
	events := s.pendingEvents
	s.pendingEvents = nil
	s.mu.Unlock()
	defer s.eventMu.Unlock()
	for _, ev := range events {
		s.h.Emit(ev)
	}
}

// emitLocked records events under lockEvents; unlockEvents delivers them after
// releasing mu. Batches are per notification/final flush, never a growing queue.
func (s *Session) emitLocked(kind event.Type, data any) {
	if s.h == nil {
		return
	}
	if s.active != nil && s.active.compact {
		switch kind {
		case event.MessageDelta, event.MessageCompleted, event.ReasoningDelta, event.ReasoningCompleted, event.ToolStarted, event.ToolCompleted:
			return
		}
	}
	turnID := ""
	if s.active != nil {
		turnID = s.active.id
	}
	s.pendingEvents = append(s.pendingEvents, event.Event{Type: kind, TurnID: turnID, Data: data})
}

func (s *Session) raw(kind string, payload json.RawMessage) {
	s.lockEvents()
	defer s.unlockEvents()
	s.rawLocked(kind, payload)
}
func (s *Session) rawLocked(kind string, payload json.RawMessage) {
	if s.cfg.RawEvents {
		s.emitLocked(event.RuntimeRaw, event.RawData{Runtime: s.rt.name, Kind: kind, Payload: append(json.RawMessage(nil), payload...)})
	}
}

func (s *Session) update(params json.RawMessage) {
	var envelope struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if json.Unmarshal(params, &envelope) != nil {
		return
	}
	var update struct {
		Kind      string `json:"sessionUpdate"`
		MessageID string `json:"messageId"`
		Content   struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		ConfigOptions     []ConfigOption `json:"configOptions"`
		AvailableCommands []struct {
			Name string `json:"name"`
		} `json:"availableCommands"`
		CompactionID string `json:"compactionId"`
		Status       string `json:"status"`
		Error        string `json:"error"`
		Used, Size   *int64
		Cost         *struct {
			Amount   float64 `json:"amount"`
			Currency string  `json:"currency"`
		} `json:"cost"`
	}
	if json.Unmarshal(envelope.Update, &update) != nil {
		return
	}
	s.lockEvents()
	defer s.unlockEvents()
	// Agents may advertise commands before session/new's response is read.
	// Retain only this capability; session ID matching gates its later use.
	if envelope.SessionID != "" && update.Kind == "available_commands_update" && (s.id == "" || envelope.SessionID == s.id) {
		s.compactCommand = false
		s.commandSessionID = envelope.SessionID
		for _, command := range update.AvailableCommands {
			if command.Name == "compact" {
				s.compactCommand = true
			}
		}
		if s.id != "" {
			s.emitLocked(event.RuntimeStatus, event.RuntimeStatusData{Status: update.Kind, Detail: envelope.Update})
		}
		return
	}
	if envelope.SessionID == "" || envelope.SessionID != s.id {
		return
	}
	if update.Kind == "config_option_update" {
		s.configOptions = update.ConfigOptions
		s.emitLocked(event.RuntimeStatus, event.RuntimeStatusData{Status: "config_options", Detail: envelope.Update})
		return
	}
	// session/load replays history. Harness Kit owns its saved transcript;
	// replaying native messages here would append duplicate events on wakeup.
	if s.active == nil {
		if s.replaying || (update.Kind != "usage_update" && update.Kind != "compaction_update") {
			return
		}
	}
	switch update.Kind {
	case "agent_message_chunk", "agent_thought_chunk":
		if update.Content.Type != "text" {
			s.rawLocked("session/update", params)
			return
		}
		state := s.active
		id := update.MessageID
		if state.kind != update.Kind || (id != "" && id != state.itemID) {
			s.finishTextLocked()
		}
		if state.itemID == "" {
			state.itemSeq++
			if id == "" {
				id = fmt.Sprintf("%s_acp_%d", state.id, state.itemSeq)
			}
			state.itemID = id
			state.kind = update.Kind
		}
		if update.Kind == "agent_message_chunk" {
			s.emitLocked(event.MessageDelta, event.MessageDeltaData{ItemID: state.itemID, Text: update.Content.Text})
		} else {
			s.emitLocked(event.ReasoningDelta, event.ReasoningDeltaData{ItemID: state.itemID, Text: update.Content.Text})
		}
		remaining := s.rt.maxRetainedOutput - len(state.text)
		if len(update.Content.Text) > remaining {
			state.text += bounded(update.Content.Text, remaining)
			state.truncated = true
		} else {
			state.text += update.Content.Text
		}
	case "tool_call", "tool_call_update":
		s.finishTextLocked()
		var tool toolUpdate
		if json.Unmarshal(envelope.Update, &tool) == nil {
			s.toolLocked(tool)
		}
		s.rawLocked("session/update", params)
	case "usage_update":
		usage := event.UsageData{Scope: "session"}
		if update.Size != nil && *update.Size >= 0 {
			usage.ContextWindow = *update.Size
		}
		if update.Cost != nil && update.Cost.Currency == "USD" && update.Cost.Amount >= 0 {
			cost := update.Cost.Amount
			usage.CostUSD = &cost
		}
		s.emitLocked(event.Usage, usage)
		s.emitLocked(event.RuntimeStatus, event.RuntimeStatusData{Status: "context_usage", Detail: envelope.Update})
		// ACP used/size describe current context, not cumulative spend.
		if update.Used != nil && *update.Used >= 0 && update.Size != nil && *update.Size > 0 {
			s.emitLocked(event.ContextUsage, event.ContextUsageData{UsedTokens: update.Used, WindowTokens: update.Size, Basis: "reported"})
		}
	case "compaction_update":
		// https://agentclientprotocol.com/protocol/v1/prompt-turn
		if update.CompactionID == "" {
			return
		}
		// Summary/error patches can repeat a lifecycle status. They are not
		// another compaction and must not invalidate a newer usage reading.
		if s.lastCompactionID == update.CompactionID && s.lastCompactionStatus == update.Status {
			s.rawLocked("session/update", params)
			return
		}
		s.lastCompactionID, s.lastCompactionStatus = update.CompactionID, update.Status
		switch update.Status {
		case "in_progress":
			s.emitLocked(event.RuntimeStatus, event.RuntimeStatusData{Status: "compacting", Detail: envelope.Update})
		case "completed":
			if s.active != nil {
				s.active.compactionConfirmed = true
			}
			s.emitLocked(event.RuntimeStatus, event.RuntimeStatusData{Status: "compacted", Detail: envelope.Update})
			s.emitLocked(event.ContextUsage, event.ContextUsageData{Basis: "unknown"})
		case "failed", "cancelled":
			if s.active != nil {
				s.active.compactionCancelled = update.Status == "cancelled"
				s.active.compactionFailure = update.Status
				if update.Error != "" {
					s.active.compactionFailure += ": " + update.Error
				}
			}
			s.emitLocked(event.RuntimeStatus, event.RuntimeStatusData{Status: "compaction_" + update.Status, Message: update.Error, Detail: envelope.Update})
		default:
			s.rawLocked("session/update", params)
		}
	case "plan", "current_mode_update", "session_info_update":
		s.emitLocked(event.RuntimeStatus, event.RuntimeStatusData{Status: update.Kind, Detail: envelope.Update})
	default:
		s.rawLocked("session/update", params)
	}
}

func (s *Session) finishTextLocked() {
	state := s.active
	if state == nil || state.itemID == "" {
		return
	}
	if state.truncated {
		s.emitLocked(event.RuntimeStatus, event.RuntimeStatusData{Status: "output_truncated", Message: "ACP completed snapshot reached the configured memory limit; streaming deltas contain the full output."})
	}
	if state.kind == "agent_message_chunk" {
		s.emitLocked(event.MessageCompleted, event.MessageCompletedData{ItemID: state.itemID, Role: "assistant", Text: state.text})
	} else {
		s.emitLocked(event.ReasoningCompleted, event.ReasoningCompletedData{ItemID: state.itemID, Text: state.text})
	}
	state.itemID, state.kind, state.text = "", "", ""
	state.truncated = false
}

func (s *Session) toolLocked(update toolUpdate) {
	if update.ID == "" {
		return
	}
	state := s.active.tools[update.ID]
	fresh := state == nil
	if state != nil && state.done {
		return
	}
	if fresh {
		state = &toolState{}
		s.active.tools[update.ID] = state
	}
	if update.Name != nil {
		state.name = *update.Name
	}
	if update.Title != nil {
		state.title = *update.Title
	}
	if update.Kind != nil {
		state.kind = *update.Kind
	}
	if update.Status != nil {
		state.status = *update.Status
	}
	if fresh {
		s.emitLocked(event.ToolStarted, event.ToolStartedData{CallID: update.ID, Name: state.name, Title: state.title, Kind: toolKind(state.kind), Input: update.RawInput})
	}
	output := ""
	hasOutput := update.Content != nil
	for _, content := range update.Content {
		if content.Type == "content" && content.Content.Type == "text" {
			if output != "" {
				output += "\n"
			}
			output += content.Content.Text
		}
	}
	if !hasOutput && len(update.RawOutput) > 0 && string(update.RawOutput) != "null" {
		hasOutput = true
		if json.Unmarshal(update.RawOutput, &output) != nil {
			output = string(update.RawOutput)
		}
	}
	if hasOutput {
		if strings.HasPrefix(output, state.output) && len(output) > state.outputBytes {
			s.emitLocked(event.ToolOutput, event.ToolOutputData{CallID: update.ID, Output: output[state.outputBytes:]})
		}
		state.output = bounded(output, s.rt.maxRetainedOutput)
		state.outputBytes = len(output)
	}
	if !state.done && (state.status == "completed" || state.status == "failed") {
		state.done = true
		status := event.ToolStatusCompleted
		if state.status == "failed" {
			status = event.ToolStatusFailed
		}
		s.emitLocked(event.ToolCompleted, event.ToolCompletedData{CallID: update.ID, Name: state.name, Status: status, Output: state.output})
		state.output = ""
	}
}

func bounded(text string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(text) <= n {
		return text
	}
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	// A substring would retain the entire oversized backing allocation.
	return strings.Clone(text[:n])
}

func toolKind(kind string) event.ToolKind {
	switch kind {
	case "read":
		return event.ToolKindFileRead
	case "edit", "delete", "move":
		return event.ToolKindFileChange
	case "search":
		return event.ToolKindSearch
	case "execute":
		return event.ToolKindCommand
	case "fetch":
		return event.ToolKindWebSearch
	default:
		return event.ToolKindOther
	}
}

func (s *Session) permission(ctx context.Context, req *jsonrpc.Request, active *turnState) (any, error) {
	cancelled := map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
	var params struct {
		SessionID string     `json:"sessionId"`
		ToolCall  toolUpdate `json:"toolCall"`
		Options   []struct {
			ID   string `json:"optionId"`
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"options"`
	}
	if err := req.UnmarshalParams(&params); err != nil {
		return nil, err
	}
	if !s.matches(params.SessionID) {
		return nil, jsonrpc.NewError(jsonrpc.CodeInvalidParams, "unknown ACP session", nil)
	}
	if active == nil || ctx.Err() != nil {
		return cancelled, nil
	}
	request := permission.Request{ID: "acp_" + string(req.ID), TurnID: active.id, Runtime: s.rt.name, Cwd: s.cwd, Kind: permission.KindTool, Input: params.ToolCall.RawInput, Outcomes: []permission.Outcome{permission.Abort}}
	if params.ToolCall.Title != nil {
		request.Title = *params.ToolCall.Title
	}
	if params.ToolCall.Name != nil {
		request.Tool = *params.ToolCall.Name
	}
	if params.ToolCall.Kind != nil {
		switch *params.ToolCall.Kind {
		case "execute":
			request.Kind = permission.KindCommand
		case "edit", "delete", "move":
			request.Kind = permission.KindFileChange
		case "fetch":
			request.Kind = permission.KindNetwork
		}
	}
	for _, location := range params.ToolCall.Locations {
		request.Paths = append(request.Paths, location.Path)
	}
	var input struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(request.Input, &input) == nil {
		request.Command = input.Command
	}
	choices := map[permission.Outcome]string{}
	for _, option := range params.Options {
		// ACP "always" choices may persist globally. Never broaden the kit's
		// session-scoped consent to that agent-defined lifetime.
		outcome := permission.Outcome("")
		switch option.Kind {
		case "allow_once":
			outcome = permission.Allow
		case "reject_once":
			outcome = permission.Deny
		}
		if outcome != "" && option.ID != "" {
			if _, exists := choices[outcome]; !exists {
				choices[outcome] = option.ID
				request.Outcomes = append(request.Outcomes, outcome)
			}
		}
	}
	decision, err := s.h.RequestApproval(ctx, request)
	if err != nil || ctx.Err() != nil {
		return cancelled, nil
	}
	if decision.Outcome == permission.Abort {
		active.cancel()
		return cancelled, nil
	}
	if len(decision.UpdatedInput) > 0 {
		return cancelled, nil
	} // ACP has no input-rewrite result.
	id, ok := choices[decision.Outcome]
	if !ok {
		return cancelled, nil
	}
	return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": id}}, nil
}
