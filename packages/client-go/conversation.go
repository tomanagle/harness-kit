package harnesskit

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// ConversationOptions bounds the client projection, independently of server retention.
// Zero selects defaults: 2000 events and 4 MiB. Negative values are invalid.
type ConversationOptions struct{ MaxEvents, MaxBytes int }
type ConversationMessage struct {
	ID           string      `json:"id"`
	Seq          uint64      `json:"seq"`
	TurnID       string      `json:"turnId,omitempty"`
	ParentCallID string      `json:"parentCallId,omitempty"`
	Role         string      `json:"role"`
	Text         string      `json:"text"`
	Input        []InputPart `json:"input,omitempty"`
	Complete     bool        `json:"complete"`
}
type ConversationSnapshot struct {
	SessionID string                `json:"sessionId"`
	Seq       uint64                `json:"seq"`
	Messages  []ConversationMessage `json:"messages"`
	Subtasks  []SubagentUpdatedData `json:"subtasks"`
	Approvals []ApprovalRequest     `json:"approvals"`
	// Tools and Turns retain native typed event payloads for caller presentation.
	Tools     []HarnessEvent    `json:"tools"`
	Turns     []HarnessEvent    `json:"turns"`
	Context   *ContextUsageData `json:"context,omitempty"`
	Truncated bool              `json:"truncated"`
	Error     string            `json:"error,omitempty"`
}

// ConversationState is a bounded, concurrency-safe event projection.
type ConversationState struct {
	mu        sync.Mutex
	sessionID string
	options   ConversationOptions
	events    []HarnessEvent
	sizes     []int
	bytes     int
	seq       uint64
	truncated bool
	err       string
}

func NewConversationState(sessionID string, options ConversationOptions) (*ConversationState, error) {
	if options.MaxEvents < 0 || options.MaxBytes < 0 {
		return nil, fmt.Errorf("harness-kit: conversation limits cannot be negative")
	}
	if options.MaxEvents == 0 {
		options.MaxEvents = 2000
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = 4 * 1024 * 1024
	}
	return &ConversationState{sessionID: sessionID, options: options}, nil
}
func (s *ConversationState) Apply(event HarnessEvent) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if event.SessionID != s.sessionID || event.Seq <= s.seq || event.Type == "stream.gap" {
		return false
	}
	s.seq = event.Seq
	raw, err := json.Marshal(event)
	if err != nil {
		s.truncated = true
		return true
	}
	size := len(raw)
	if size > s.options.MaxBytes {
		s.truncated = true
		return true
	}
	event.Data = append(json.RawMessage(nil), event.Data...)
	s.events = append(s.events, event)
	s.sizes = append(s.sizes, size)
	s.bytes += size
	for len(s.events) > s.options.MaxEvents || s.bytes > s.options.MaxBytes {
		s.bytes -= s.sizes[0]
		s.events[0] = HarnessEvent{}
		s.events = s.events[1:]
		s.sizes = s.sizes[1:]
		s.truncated = true
	}
	return true
}
func (s *ConversationState) Snapshot() ConversationSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := ConversationSnapshot{SessionID: s.sessionID, Seq: s.seq, Truncated: s.truncated, Error: s.err, Messages: []ConversationMessage{}, Subtasks: []SubagentUpdatedData{}, Approvals: []ApprovalRequest{}, Tools: []HarnessEvent{}, Turns: []HarnessEvent{}}
	messages := map[string]int{}
	subtasks := map[string]int{}
	approvals := map[string]ApprovalRequest{}
	tools := map[string]int{}
	turns := map[string]int{}
	for _, event := range s.events {
		switch event.Type {
		case "turn.started":
			var data TurnStartedData
			if json.Unmarshal(event.Data, &data) != nil {
				continue
			}
			text := data.Text
			if len(data.Input) > 0 && data.Input[0].Type == "text" {
				text = data.Input[0].Text
			}
			out.Messages = append(out.Messages, ConversationMessage{Seq: event.Seq, ID: event.TurnID + ":user", TurnID: event.TurnID, Role: "user", Text: text, Input: data.Input, Complete: true})
			turns[event.TurnID] = len(out.Turns)
			out.Turns = append(out.Turns, event)
		case "message.delta", "message.completed", "reasoning.delta", "reasoning.completed":
			var data struct{ ItemID, Text, ParentCallID string }
			if json.Unmarshal(event.Data, &data) != nil {
				continue
			}
			role := "assistant"
			if event.Type == "reasoning.delta" || event.Type == "reasoning.completed" {
				role = "reasoning"
			}
			id := event.TurnID + ":" + data.ParentCallID + ":" + role + ":" + data.ItemID
			index, ok := messages[id]
			if !ok {
				index = len(out.Messages)
				messages[id] = index
				out.Messages = append(out.Messages, ConversationMessage{Seq: event.Seq, ID: id, TurnID: event.TurnID, ParentCallID: data.ParentCallID, Role: role})
			}
			message := &out.Messages[index]
			message.Complete = event.Type == "message.completed" || event.Type == "reasoning.completed"
			if message.Complete {
				message.Text = data.Text
			} else {
				message.Text += data.Text
			}
		case "subagent.updated":
			var data SubagentUpdatedData
			if json.Unmarshal(event.Data, &data) != nil {
				continue
			}
			index, ok := subtasks[data.ID]
			if !ok && data.CallID != "" {
				for i, previous := range out.Subtasks {
					if previous.CallID == data.CallID {
						index, ok = i, true
						break
					}
				}
			}
			if ok {
				previous := out.Subtasks[index]
				raw, _ := json.Marshal(previous)
				var merged map[string]any
				_ = json.Unmarshal(raw, &merged)
				var update map[string]any
				_ = json.Unmarshal(event.Data, &update)
				for k, v := range update {
					merged[k] = v
				}
				raw, _ = json.Marshal(merged)
				_ = json.Unmarshal(raw, &out.Subtasks[index])
			} else {
				index = len(out.Subtasks)
				out.Subtasks = append(out.Subtasks, data)
			}
			subtasks[data.ID] = index
		case "approval.requested":
			var data ApprovalRequest
			if json.Unmarshal(event.Data, &data) == nil {
				approvals[data.ID] = data
			}
		case "approval.resolved":
			var data ApprovalResolvedData
			if json.Unmarshal(event.Data, &data) == nil {
				delete(approvals, data.ApprovalID)
			}
		case "tool.started", "tool.completed":
			var data struct{ CallID string }
			if json.Unmarshal(event.Data, &data) != nil {
				continue
			}
			if i, ok := tools[data.CallID]; ok {
				out.Tools[i] = event
			} else {
				tools[data.CallID] = len(out.Tools)
				out.Tools = append(out.Tools, event)
			}
		case "turn.completed":
			if i, ok := turns[event.TurnID]; ok {
				out.Turns[i] = event
			} else {
				turns[event.TurnID] = len(out.Turns)
				out.Turns = append(out.Turns, event)
			}
			for id, approval := range approvals {
				if approval.TurnID == event.TurnID {
					delete(approvals, id)
				}
			}
		case "context.usage":
			var data ContextUsageData
			if json.Unmarshal(event.Data, &data) == nil {
				out.Context = &data
			}
		}
	}
	for _, approval := range approvals {
		out.Approvals = append(out.Approvals, approval)
	}
	// Each snapshot owns all its slices, raw payloads and nested values.
	raw, _ := json.Marshal(out)
	var copy ConversationSnapshot
	_ = json.Unmarshal(raw, &copy)
	return copy
}

// Conversation observes history and live changes without starting an agent.
// Updates coalesces slow readers to the newest snapshot. Close releases the observer.
type Conversation struct {
	Updates <-chan ConversationSnapshot
	state   *ConversationState
	cancel  context.CancelFunc
}

func (c *Conversation) Snapshot() ConversationSnapshot { return c.state.Snapshot() }
func (c *Conversation) Close()                         { c.cancel() }
func (c *Client) ObserveConversation(ctx context.Context, sessionID string, options ConversationOptions) (*Conversation, error) {
	state, err := NewConversationState(sessionID, options)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	wake := make(chan struct{}, 1)
	stop := c.SubscribeEvents(func(event HarnessEvent) {
		if event.SessionID == sessionID {
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	})
	replay := func() error {
		state.mu.Lock()
		cursor := state.seq
		state.mu.Unlock()
		for {
			page, err := c.SessionEventsPage(ctx, SessionEventsParams{SessionID: sessionID, AfterSeq: cursor, Limit: 200})
			if err != nil {
				return err
			}
			if page.Truncated {
				state.mu.Lock()
				state.truncated = true
				state.mu.Unlock()
			}
			for _, event := range page.Events {
				state.Apply(event)
			}
			if !page.HasMore {
				return nil
			}
			if page.NextSeq <= cursor {
				return fmt.Errorf("harness-kit: conversation history did not advance")
			}
			cursor = page.NextSeq
		}
	}
	if err := replay(); err != nil {
		stop()
		cancel()
		return nil, err
	}
	updates := make(chan ConversationSnapshot, 1)
	updates <- state.Snapshot()
	observer := &Conversation{Updates: updates, state: state, cancel: cancel}
	go func() {
		defer close(updates)
		defer stop()
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.Done():
				return
			case <-wake:
				err := replay()
				state.mu.Lock()
				state.err = ""
				if err != nil {
					state.err = err.Error()
				}
				state.mu.Unlock()
				snapshot := state.Snapshot()
				select {
				case updates <- snapshot:
				default:
					select {
					case <-updates:
					default:
					}
					select {
					case updates <- snapshot:
					default:
					}
				}
			}
		}
	}()
	return observer, nil
}
