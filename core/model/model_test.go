package model

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"testing"

	"github.com/tomanagle/harness-kit/core/config"
)

func call(id string) Part {
	return Part{Type: "tool_call", Call: &ToolCall{ID: id, Name: "test", Arguments: json.RawMessage(`{}`)}}
}

func TestToolLinkage(t *testing.T) {
	base := []Message{Text(User, "run"), {Role: Assistant, Content: []Part{call("a"), call("b")}}}
	if err := ValidateMessages(base); err != nil {
		t.Fatal(err)
	} // A snapshot may contain pending calls.
	request := Request{Settings: config.Settings{Model: "chosen-model"}, Messages: append(base, Result("a", "ok", false))}
	if err := request.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("partial results accepted: %v", err)
	}
	request.Messages = append(request.Messages, Result("b", "ok", false))
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string][]Message{
		"orphan":    {Result("missing", "ok", false)},
		"repeated":  append(append([]Message{}, request.Messages...), Result("a", "again", false)),
		"missing":   append(append([]Message{}, base...), Text(User, "next")),
		"tool text": {Text(ToolRole, "bad")},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateMessages(bad); !errors.Is(err, ErrInvalid) {
				t.Fatalf("accepted: %v", err)
			}
		})
	}
}

func TestCloneNativeAndArguments(t *testing.T) {
	input := []Message{{Role: Assistant, Content: []Part{call("a")}, Native: &Native{Provider: "test", Data: json.RawMessage(`[{"x":1}]`)}}}
	copy, err := CloneMessages(input)
	if err != nil {
		t.Fatal(err)
	}
	copy[0].Native.Data[0] = 'x'
	copy[0].Content[0].Call.Arguments[0] = 'x'
	if !json.Valid(input[0].Native.Data) || !json.Valid(input[0].Content[0].Call.Arguments) {
		t.Fatal("snapshot aliases input")
	}
}

type fakeProvider struct {
	closed   *bool
	complete bool
}

func (fakeProvider) Info() Info { return Info{Name: "test"} }
func (p fakeProvider) Stream(context.Context, Request) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		defer func() { *p.closed = true }()
		if !yield(Event{Type: TextDelta, Text: "hello"}, nil) {
			return
		}
		if p.complete {
			m := Text(Assistant, "hello")
			yield(Event{Type: Completed, Message: &m, Usage: &Usage{InputTokens: 3}}, nil)
		}
	}
}
func TestCollectClosesAndRequiresCompletion(t *testing.T) {
	closed := false
	p := fakeProvider{closed: &closed, complete: true}
	_, _, err := Collect(context.Background(), p, Request{}, func(Event) bool { return false })
	if !errors.Is(err, context.Canceled) || !closed {
		t.Fatalf("early stop: closed=%v err=%v", closed, err)
	}
	p.complete = false
	_, _, err = Collect(context.Background(), p, Request{}, nil)
	if !errors.Is(err, ErrIncomplete) {
		t.Fatal(err)
	}
	p.complete = true
	m, u, err := Collect(context.Background(), p, Request{}, nil)
	if err != nil || m.Text() != "hello" || u.InputTokens != 3 {
		t.Fatalf("%+v %+v %v", m, u, err)
	}
}
