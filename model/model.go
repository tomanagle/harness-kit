// Package model defines raw streaming inference for applications that own their
// agent loop. Providers never execute tools or choose a model.
// Provider SDK adapters live in separate modules under providers/.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"

	"github.com/tomanagle/harness-kit/config"
)

var (
	ErrInvalid     = errors.New("model: invalid request")
	ErrUnsupported = errors.New("model: unsupported input")
	ErrIncomplete  = errors.New("model: stream ended without completion")
)

type Provider interface {
	Info() Info
	Stream(context.Context, Request) iter.Seq2[Event, error]
}

type Info struct {
	Name      string `json:"name"`
	Images    bool   `json:"images"`
	Tools     bool   `json:"tools"`
	Reasoning bool   `json:"reasoning"`
}

type Request struct {
	// Settings uses the same model/effort/native option selection as sessions.
	// Model is required; adapters do not select a paid model on the caller's behalf.
	Settings config.Settings `json:"settings"`
	Messages []Message       `json:"messages"`
	Tools    []Tool          `json:"tools,omitempty"`
	// MaxOutputTokens is a caller-selected cap. Anthropic requires a positive
	// value; OpenAI may leave it zero to use the API's default.
	MaxOutputTokens int64 `json:"maxOutputTokens,omitempty"`
}

type Role string

const (
	System    Role = "system"
	Developer Role = "developer"
	User      Role = "user"
	Assistant Role = "assistant"
	ToolRole  Role = "tool"
)

type Message struct {
	Role    Role   `json:"role"`
	Content []Part `json:"content"`
	// Native preserves provider-owned reasoning signatures, encrypted state and
	// other output fields required for replay. When present, matching adapters
	// replay this instead of reconstructing Content. Cross-provider replay must
	// be deliberately transformed by the host; it is never silently stripped.
	Native *Native `json:"native,omitempty"`
}
type Native struct {
	Provider string          `json:"provider"`
	Data     json.RawMessage `json:"data"`
}
type Part struct {
	Type   string      `json:"type"` // text, image, tool_call, tool_result, reasoning
	Text   string      `json:"text,omitempty"`
	Image  *Image      `json:"image,omitempty"`
	Call   *ToolCall   `json:"call,omitempty"`
	Result *ToolResult `json:"result,omitempty"`
}
type Image struct {
	// Supply exactly one of URL or base64 Data. The kit does not read files or
	// fetch URLs; the owning application controls those operations.
	URL       string `json:"url,omitempty"`
	Data      string `json:"data,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
}
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}
type ToolResult struct {
	CallID  string `json:"callId"`
	Text    string `json:"text"`
	IsError bool   `json:"isError,omitempty"`
}
type Usage struct {
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int64 `json:"cacheWriteTokens,omitempty"`
	ReasoningTokens  int64 `json:"reasoningTokens,omitempty"`
}
type EventType string

const (
	TextDelta        EventType = "text.delta"
	ReasoningDelta   EventType = "reasoning.delta"
	ToolCallDelta    EventType = "tool_call.delta"
	ToolCallComplete EventType = "tool_call.completed"
	UsageUpdated     EventType = "usage"
	Completed        EventType = "completed"
)

type Event struct {
	Type EventType `json:"type"`
	Text string    `json:"text,omitempty"`
	// ArgumentsDelta is an unparsed JSON fragment on ToolCallDelta events.
	// Call.Arguments is populated only for complete, valid argument objects.
	ArgumentsDelta string    `json:"argumentsDelta,omitempty"`
	Call           *ToolCall `json:"call,omitempty"`
	Usage          *Usage    `json:"usage,omitempty"`
	Message        *Message  `json:"message,omitempty"`
	ResponseID     string    `json:"responseId,omitempty"`
	FinishReason   string    `json:"finishReason,omitempty"`
}

func Text(role Role, text string) Message {
	return Message{Role: role, Content: []Part{{Type: "text", Text: text}}}
}
func Result(callID, text string, isError bool) Message {
	return Message{Role: ToolRole, Content: []Part{{Type: "tool_result", Result: &ToolResult{CallID: callID, Text: text, IsError: isError}}}}
}
func (m Message) Text() string {
	var b strings.Builder
	for _, p := range m.Content {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
func (m Message) Calls() []ToolCall {
	var calls []ToolCall
	for _, p := range m.Content {
		if p.Type == "tool_call" && p.Call != nil {
			calls = append(calls, *p.Call)
		}
	}
	return calls
}

// CloneMessages gives callers independent snapshots, including native blocks.
func CloneMessages(messages []Message) ([]Message, error) {
	b, err := json.Marshal(messages)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var out []Message
	err = json.Unmarshal(b, &out)
	return out, err
}

// ValidateMessages checks portable tool-call linkage. A tool exchange must be
// completed before another user/assistant message, except a trailing assistant
// tool-call message which is valid while the host is executing its tools.
func ValidateMessages(messages []Message) error {
	return validateMessages(messages, false)
}

func validateMessages(messages []Message, requireResults bool) error {
	pending := map[string]bool{}
	used := map[string]bool{}
	for i, m := range messages {
		switch m.Role {
		case System, Developer, User, Assistant, ToolRole:
		default:
			return fmt.Errorf("%w: message %d has unknown role %q", ErrInvalid, i, m.Role)
		}
		if m.Native != nil && (m.Native.Provider == "" || !json.Valid(m.Native.Data)) {
			return fmt.Errorf("%w: invalid native message", ErrInvalid)
		}
		hasResults := false
		for _, p := range m.Content {
			if p.Type == "tool_result" {
				hasResults = true
			}
		}
		if len(pending) > 0 && !hasResults {
			return fmt.Errorf("%w: tool results missing before message %d", ErrInvalid, i)
		}
		for _, p := range m.Content {
			if m.Role == ToolRole && p.Type != "tool_result" {
				return fmt.Errorf("%w: tool messages contain only results", ErrInvalid)
			}
			switch p.Type {
			case "text", "reasoning":
			case "image":
				if p.Image == nil || (p.Image.URL == "") == (p.Image.Data == "") || p.Image.Data != "" && p.Image.MediaType == "" {
					return fmt.Errorf("%w: image requires one URL or base64 data and media type", ErrInvalid)
				}
			case "tool_call":
				if m.Role != Assistant || p.Call == nil || p.Call.ID == "" || p.Call.Name == "" || !json.Valid(p.Call.Arguments) || used[p.Call.ID] {
					return fmt.Errorf("%w: invalid or duplicate tool call", ErrInvalid)
				}
				pending[p.Call.ID] = true
				used[p.Call.ID] = true
			case "tool_result":
				if m.Role != ToolRole && m.Role != User {
					return fmt.Errorf("%w: tool result must have user/tool role", ErrInvalid)
				}
				if p.Result == nil || !pending[p.Result.CallID] {
					return fmt.Errorf("%w: orphan or repeated tool result", ErrInvalid)
				}
				delete(pending, p.Result.CallID)
			default:
				return fmt.Errorf("%w: content type %q", ErrUnsupported, p.Type)
			}
		}
	}
	if requireResults && len(pending) > 0 {
		return fmt.Errorf("%w: provide all tool results before continuing inference", ErrInvalid)
	}
	return nil
}

func (r Request) Validate() error {
	if strings.TrimSpace(r.Settings.Model) == "" {
		return fmt.Errorf("%w: settings.model is required", ErrInvalid)
	}
	if r.MaxOutputTokens < 0 {
		return fmt.Errorf("%w: negative output limit", ErrInvalid)
	}
	if len(r.Messages) == 0 {
		return fmt.Errorf("%w: messages are required", ErrInvalid)
	}
	if err := validateMessages(r.Messages, true); err != nil {
		return err
	}
	names := map[string]bool{}
	for _, tool := range r.Tools {
		var schema map[string]any
		if tool.Name == "" || names[tool.Name] || json.Unmarshal(tool.InputSchema, &schema) != nil || schema == nil {
			return fmt.Errorf("%w: invalid/duplicate tool definition %q", ErrInvalid, tool.Name)
		}
		names[tool.Name] = true
	}
	for key, value := range r.Settings.Options {
		if key == "" || !json.Valid(value) {
			return fmt.Errorf("%w: invalid native option %q", ErrInvalid, key)
		}
	}
	return nil
}

// Collect consumes a provider stream, optionally observing events. The final
// assistant message retains native continuation data. Stopping observe closes
// the provider stream and returns context.Canceled; no tools are executed.
func Collect(ctx context.Context, p Provider, r Request, observe func(Event) bool) (Message, Usage, error) {
	var message Message
	var usage Usage
	completed := false
	for ev, err := range p.Stream(ctx, r) {
		if err != nil {
			return message, usage, err
		}
		if ev.Usage != nil {
			usage = *ev.Usage
		}
		if ev.Type == Completed && ev.Message != nil {
			message = *ev.Message
			completed = true
		}
		if observe != nil && !observe(ev) {
			return message, usage, context.Canceled
		}
	}
	if !completed {
		return message, usage, ErrIncomplete
	}
	return message, usage, nil
}
