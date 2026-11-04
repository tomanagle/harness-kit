// Package anthropic adapts the official Anthropic Go SDK Messages streaming API
// to model.Provider. Credentials and transport remain host-owned SDK options.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/tomanagle/harness-kit/core/model"
)

type Provider struct{ client sdk.Client }

func New(opts ...option.RequestOption) *Provider {
	return &Provider{client: sdk.NewClient(append([]option.RequestOption{option.WithMaxRetries(0)}, opts...)...)}
}
func NewWithClient(client sdk.Client) *Provider { return &Provider{client: client} }
func (*Provider) Info() model.Info {
	return model.Info{Name: "anthropic", Images: true, Tools: true, Reasoning: true}
}

func (p *Provider) Stream(ctx context.Context, r model.Request) iter.Seq2[model.Event, error] {
	return func(yield func(model.Event, error) bool) {
		body, err := requestBody(r)
		if err != nil {
			yield(model.Event{}, err)
			return
		}
		stream := p.client.Messages.NewStreaming(ctx, param.Override[sdk.MessageNewParams](body))
		defer stream.Close()
		var acc sdk.Message
		calls := map[int]*model.ToolCall{}
		started := map[int]bool{}
		complete := false
		for stream.Next() {
			current := stream.Current()
			var ev struct {
				Type         string          `json:"type"`
				Index        int             `json:"index"`
				ContentBlock json.RawMessage `json:"content_block"`
				Delta        struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					Thinking    string `json:"thinking"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
				Error json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal([]byte(current.RawJSON()), &ev); err != nil {
				yield(model.Event{}, err)
				return
			}
			if ev.Type == "error" {
				yield(model.Event{}, fmt.Errorf("anthropic: stream error: %s", ev.Error))
				return
			}
			if ev.Type == "content_block_start" {
				var block contentBlock
				if err := json.Unmarshal(ev.ContentBlock, &block); err != nil {
					yield(model.Event{}, err)
					return
				}
				if block.Type == "tool_use" {
					calls[ev.Index] = &model.ToolCall{ID: block.ID, Name: block.Name, Arguments: block.Input}
				}
			}
			if ev.Type == "content_block_delta" && ev.Delta.Type == "input_json_delta" {
				call := calls[ev.Index]
				if call == nil {
					yield(model.Event{}, fmt.Errorf("anthropic: tool delta references unknown block %d", ev.Index))
					return
				}
				if !started[ev.Index] {
					call.Arguments = nil
					started[ev.Index] = true
				}
				call.Arguments = append(call.Arguments, ev.Delta.PartialJSON...)
				copy := *call
				copy.Arguments = nil
				if !yield(model.Event{Type: model.ToolCallDelta, Call: &copy, ArgumentsDelta: ev.Delta.PartialJSON}, nil) {
					return
				}
			}
			if ev.Type == "content_block_stop" {
				if call := calls[ev.Index]; call != nil {
					if !json.Valid(call.Arguments) {
						yield(model.Event{}, fmt.Errorf("anthropic: incomplete tool arguments"))
						return
					}
					copy := *call
					copy.Arguments = append(json.RawMessage(nil), call.Arguments...)
					if !yield(model.Event{Type: model.ToolCallComplete, Call: &copy}, nil) {
						return
					}
				}
			}
			if err := acc.Accumulate(current); err != nil {
				yield(model.Event{}, err)
				return
			}
			switch ev.Type {
			case "content_block_delta":
				switch ev.Delta.Type {
				case "text_delta":
					if !yield(model.Event{Type: model.TextDelta, Text: ev.Delta.Text}, nil) {
						return
					}
				case "thinking_delta":
					if !yield(model.Event{Type: model.ReasoningDelta, Text: ev.Delta.Thinking}, nil) {
						return
					}
				}
			case "message_start", "message_delta":
				usage := usageOf(acc)
				if !yield(model.Event{Type: model.UsageUpdated, Usage: &usage, ResponseID: acc.ID}, nil) {
					return
				}
			case "message_stop":
				message, err := messageOf(acc)
				if err != nil {
					yield(model.Event{}, err)
					return
				}
				usage := usageOf(acc)
				reason := string(acc.StopReason)
				if reason == "end_turn" {
					reason = "stop"
				} else if reason == "tool_use" {
					reason = "tool_calls"
				}
				if !yield(model.Event{Type: model.Completed, Message: &message, Usage: &usage, ResponseID: acc.ID, FinishReason: reason}, nil) {
					return
				}
				complete = true
			}
		}
		if err := stream.Err(); err != nil {
			yield(model.Event{}, err)
			return
		}
		if !complete {
			yield(model.Event{}, model.ErrIncomplete)
		}
	}
}

func requestBody(r model.Request) (map[string]any, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if r.MaxOutputTokens <= 0 {
		return nil, fmt.Errorf("%w: Anthropic requires maxOutputTokens > 0", model.ErrInvalid)
	}
	system := []any{}
	messages := []any{}
	conversationStarted := false
	for _, m := range r.Messages {
		if m.Role == model.System || m.Role == model.Developer {
			if conversationStarted {
				return nil, fmt.Errorf("%w: Anthropic portable instructions must precede conversation messages", model.ErrUnsupported)
			}
			if m.Native != nil {
				return nil, fmt.Errorf("%w: native instruction content", model.ErrUnsupported)
			}
			for _, p := range m.Content {
				if p.Type != "text" {
					return nil, fmt.Errorf("%w: Anthropic instructions must be text", model.ErrUnsupported)
				}
				system = append(system, map[string]any{"type": "text", "text": p.Text})
			}
			continue
		}
		conversationStarted = true
		role := string(m.Role)
		if m.Role == model.ToolRole {
			role = "user"
		}
		if m.Native != nil {
			if m.Native.Provider != "anthropic" {
				return nil, fmt.Errorf("%w: native %s history cannot be replayed by Anthropic", model.ErrUnsupported, m.Native.Provider)
			}
			var blocks []json.RawMessage
			if err := json.Unmarshal(m.Native.Data, &blocks); err != nil {
				return nil, fmt.Errorf("%w: invalid Anthropic native history", model.ErrInvalid)
			}
			messages = append(messages, map[string]any{"role": role, "content": blocks})
			continue
		}
		parts := []any{}
		for _, p := range m.Content {
			switch p.Type {
			case "text":
				parts = append(parts, map[string]any{"type": "text", "text": p.Text})
			case "image":
				source := map[string]any{"type": "url", "url": p.Image.URL}
				if p.Image.Data != "" {
					source = map[string]any{"type": "base64", "media_type": p.Image.MediaType, "data": p.Image.Data}
				}
				parts = append(parts, map[string]any{"type": "image", "source": source})
			case "tool_call":
				parts = append(parts, map[string]any{"type": "tool_use", "id": p.Call.ID, "name": p.Call.Name, "input": p.Call.Arguments})
			case "tool_result":
				parts = append(parts, map[string]any{"type": "tool_result", "tool_use_id": p.Result.CallID, "content": p.Result.Text, "is_error": p.Result.IsError})
			case "reasoning":
				return nil, fmt.Errorf("%w: thinking replay requires signed native content", model.ErrUnsupported)
			}
		}
		messages = append(messages, map[string]any{"role": role, "content": parts})
	}
	body := map[string]any{"model": r.Settings.Model, "messages": messages, "max_tokens": r.MaxOutputTokens}
	if len(system) > 0 {
		body["system"] = system
	}
	if r.Settings.Effort != "" {
		body["output_config"] = map[string]any{"effort": r.Settings.Effort}
	}
	if len(r.Tools) > 0 {
		tools := []any{}
		for _, t := range r.Tools {
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "input_schema": t.InputSchema})
		}
		body["tools"] = tools
	}
	for key, value := range r.Settings.Options {
		switch key {
		case "model", "messages", "system", "tools", "stream", "max_tokens":
			return nil, fmt.Errorf("%w: use portable field instead of options.%s", model.ErrInvalid, key)
		case "output_config":
			if r.Settings.Effort != "" {
				return nil, fmt.Errorf("%w: effort conflicts with options.output_config", model.ErrInvalid)
			}
		}
		body[key] = value
	}
	return body, nil
}

func usageOf(m sdk.Message) model.Usage {
	return model.Usage{InputTokens: m.Usage.InputTokens + m.Usage.CacheReadInputTokens + m.Usage.CacheCreationInputTokens, OutputTokens: m.Usage.OutputTokens, CacheReadTokens: m.Usage.CacheReadInputTokens, CacheWriteTokens: m.Usage.CacheCreationInputTokens}
}

type contentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
}

func messageOf(m sdk.Message) (model.Message, error) {
	out := model.Message{Role: model.Assistant, Content: []model.Part{}}
	native := []json.RawMessage{}
	for _, c := range m.Content {
		raw := json.RawMessage(c.RawJSON())
		native = append(native, raw)
		var b contentBlock
		if err := json.Unmarshal(raw, &b); err != nil {
			return out, err
		}
		switch b.Type {
		case "text":
			out.Content = append(out.Content, model.Part{Type: "text", Text: b.Text})
		case "thinking":
			out.Content = append(out.Content, model.Part{Type: "reasoning", Text: b.Thinking})
		case "tool_use":
			if !json.Valid(b.Input) {
				return out, fmt.Errorf("anthropic: malformed final tool arguments")
			}
			out.Content = append(out.Content, model.Part{Type: "tool_call", Call: &model.ToolCall{ID: b.ID, Name: b.Name, Arguments: b.Input}})
		}
	}
	data, err := json.Marshal(native)
	if err != nil {
		return out, err
	}
	out.Native = &model.Native{Provider: "anthropic", Data: data}
	return out, nil
}
