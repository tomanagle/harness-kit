// Package openai adapts the official OpenAI Go SDK Responses streaming API to
// model.Provider. Credentials and HTTP behavior are supplied through SDK options.
package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/tomanagle/harness-kit/model"
)

type Provider struct{ client sdk.Client }

// New uses the SDK's credential configuration (including OPENAI_API_KEY).
// Retries are off unless the host explicitly supplies WithMaxRetries: a library
// must not silently replay an inference request on an ambiguous failure.
func New(opts ...option.RequestOption) *Provider {
	return &Provider{client: sdk.NewClient(append([]option.RequestOption{option.WithMaxRetries(0)}, opts...)...)}
}
func NewWithClient(client sdk.Client) *Provider { return &Provider{client: client} }
func (*Provider) Info() model.Info {
	return model.Info{Name: "openai", Images: true, Tools: true, Reasoning: true}
}

func (p *Provider) Stream(ctx context.Context, r model.Request) iter.Seq2[model.Event, error] {
	return func(yield func(model.Event, error) bool) {
		body, err := requestBody(r)
		if err != nil {
			yield(model.Event{}, err)
			return
		}
		stream := p.client.Responses.NewStreaming(ctx, param.Override[responses.ResponseNewParams](body))
		defer stream.Close()
		calls := map[string]*model.ToolCall{}
		complete := false
		for stream.Next() {
			var ev struct {
				Type      string          `json:"type"`
				Delta     string          `json:"delta"`
				ItemID    string          `json:"item_id"`
				Arguments string          `json:"arguments"`
				Item      json.RawMessage `json:"item"`
				Response  json.RawMessage `json:"response"`
				Error     json.RawMessage `json:"error"`
				Message   string          `json:"message"`
			}
			if err := json.Unmarshal([]byte(stream.Current().RawJSON()), &ev); err != nil {
				yield(model.Event{}, err)
				return
			}
			switch ev.Type {
			case "response.output_text.delta":
				if !yield(model.Event{Type: model.TextDelta, Text: ev.Delta}, nil) {
					return
				}
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				if !yield(model.Event{Type: model.ReasoningDelta, Text: ev.Delta}, nil) {
					return
				}
			case "response.output_item.added":
				var item outputItem
				if err := json.Unmarshal(ev.Item, &item); err != nil {
					yield(model.Event{}, err)
					return
				}
				if item.Type == "function_call" {
					calls[item.ID] = &model.ToolCall{ID: item.CallID, Name: item.Name, Arguments: json.RawMessage(item.Arguments)}
				}
			case "response.function_call_arguments.delta":
				call := calls[ev.ItemID]
				if call == nil {
					yield(model.Event{}, fmt.Errorf("openai: tool delta references unknown item %s", ev.ItemID))
					return
				}
				call.Arguments = append(call.Arguments, ev.Delta...)
				copy := *call
				copy.Arguments = nil
				if !yield(model.Event{Type: model.ToolCallDelta, Call: &copy, ArgumentsDelta: ev.Delta}, nil) {
					return
				}
			case "response.output_item.done":
				var item outputItem
				if err := json.Unmarshal(ev.Item, &item); err != nil {
					yield(model.Event{}, err)
					return
				}
				if item.Type == "function_call" {
					call := model.ToolCall{ID: item.CallID, Name: item.Name, Arguments: json.RawMessage(item.Arguments)}
					if !json.Valid(call.Arguments) {
						yield(model.Event{}, fmt.Errorf("openai: malformed tool arguments"))
						return
					}
					if !yield(model.Event{Type: model.ToolCallComplete, Call: &call}, nil) {
						return
					}
				}
			case "response.completed", "response.incomplete":
				message, usage, id, reason, err := completed(ev.Response)
				if err != nil {
					yield(model.Event{}, err)
					return
				}
				if !yield(model.Event{Type: model.UsageUpdated, Usage: &usage, ResponseID: id}, nil) {
					return
				}
				if !yield(model.Event{Type: model.Completed, Message: &message, Usage: &usage, ResponseID: id, FinishReason: reason}, nil) {
					return
				}
				complete = true
			case "response.failed", "error":
				detail := ev.Message
				if detail == "" {
					detail = string(ev.Error)
				}
				if detail == "" || detail == "null" {
					detail = string(ev.Response)
				}
				yield(model.Event{}, fmt.Errorf("openai: %s: %s", ev.Type, detail))
				return
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
	input := []any{}
	for _, m := range r.Messages {
		if m.Native != nil {
			if m.Native.Provider != "openai" {
				return nil, fmt.Errorf("%w: native %s history cannot be replayed by OpenAI", model.ErrUnsupported, m.Native.Provider)
			}
			var items []json.RawMessage
			if err := json.Unmarshal(m.Native.Data, &items); err != nil {
				return nil, fmt.Errorf("%w: invalid OpenAI native history", model.ErrInvalid)
			}
			for _, item := range items {
				input = append(input, item)
			}
			continue
		}
		parts := []any{}
		flush := func() {
			if len(parts) > 0 {
				input = append(input, map[string]any{"type": "message", "role": string(m.Role), "content": parts})
				parts = nil
			}
		}
		for _, part := range m.Content {
			switch part.Type {
			case "text":
				parts = append(parts, map[string]any{"type": "input_text", "text": part.Text})
			case "image":
				url := part.Image.URL
				if url == "" {
					url = "data:" + part.Image.MediaType + ";base64," + part.Image.Data
				}
				parts = append(parts, map[string]any{"type": "input_image", "image_url": url})
			case "tool_call":
				flush()
				input = append(input, map[string]any{"type": "function_call", "call_id": part.Call.ID, "name": part.Call.Name, "arguments": string(part.Call.Arguments)})
			case "tool_result":
				flush()
				text := part.Result.Text
				if part.Result.IsError {
					text = "Tool error: " + text
				}
				input = append(input, map[string]any{"type": "function_call_output", "call_id": part.Result.CallID, "output": text})
			case "reasoning":
				return nil, fmt.Errorf("%w: reasoning replay requires native output items", model.ErrUnsupported)
			}
		}
		flush()
	}
	body := map[string]any{"model": r.Settings.Model, "input": input}
	if r.MaxOutputTokens > 0 {
		body["max_output_tokens"] = r.MaxOutputTokens
	}
	if r.Settings.Effort != "" {
		body["reasoning"] = map[string]any{"effort": r.Settings.Effort}
	}
	if len(r.Tools) > 0 {
		tools := []any{}
		for _, t := range r.Tools {
			tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": t.InputSchema, "strict": false})
		}
		body["tools"] = tools
	}
	for key, value := range r.Settings.Options {
		switch key {
		case "model", "input", "tools", "stream", "max_output_tokens":
			return nil, fmt.Errorf("%w: use portable field instead of options.%s", model.ErrInvalid, key)
		case "reasoning":
			if r.Settings.Effort != "" {
				return nil, fmt.Errorf("%w: effort conflicts with options.reasoning", model.ErrInvalid)
			}
		}
		body[key] = value
	}
	return body, nil
}

type outputItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
}

func completed(raw json.RawMessage) (model.Message, model.Usage, string, string, error) {
	var response struct {
		ID         string            `json:"id"`
		Status     string            `json:"status"`
		Output     []json.RawMessage `json:"output"`
		Incomplete struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Usage struct {
			Input        int64 `json:"input_tokens"`
			Output       int64 `json:"output_tokens"`
			InputDetails struct {
				Cached int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputDetails struct {
				Reasoning int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return model.Message{}, model.Usage{}, "", "", err
	}
	message := model.Message{Role: model.Assistant, Content: []model.Part{}}
	native, _ := json.Marshal(response.Output)
	message.Native = &model.Native{Provider: "openai", Data: native}
	reason := "stop"
	for _, raw := range response.Output {
		var item outputItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return message, model.Usage{}, "", "", err
		}
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" {
					message.Content = append(message.Content, model.Part{Type: "text", Text: c.Text})
				} else if c.Type == "refusal" {
					message.Content = append(message.Content, model.Part{Type: "text", Text: c.Refusal})
					reason = "refusal"
				}
			}
		case "function_call":
			if !json.Valid([]byte(item.Arguments)) {
				return message, model.Usage{}, "", "", fmt.Errorf("openai: malformed completed tool arguments")
			}
			message.Content = append(message.Content, model.Part{Type: "tool_call", Call: &model.ToolCall{ID: item.CallID, Name: item.Name, Arguments: json.RawMessage(item.Arguments)}})
			reason = "tool_calls"
		case "reasoning":
			var text strings.Builder
			for _, s := range item.Summary {
				text.WriteString(s.Text)
			}
			message.Content = append(message.Content, model.Part{Type: "reasoning", Text: text.String()})
		}
	}
	if response.Status == "incomplete" {
		reason = response.Incomplete.Reason
		if reason == "" {
			reason = "incomplete"
		}
	}
	usage := model.Usage{InputTokens: response.Usage.Input, OutputTokens: response.Usage.Output, CacheReadTokens: response.Usage.InputDetails.Cached, ReasoningTokens: response.Usage.OutputDetails.Reasoning}
	return message, usage, response.ID, reason, nil
}
