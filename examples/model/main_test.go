package main

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"testing"

	"github.com/tomanagle/harness-kit/core/config"
	"github.com/tomanagle/harness-kit/core/mcp"
	"github.com/tomanagle/harness-kit/core/model"
	"github.com/tomanagle/harness-kit/core/model/contextwindow"
	"github.com/tomanagle/harness-kit/core/tools"
)

type fake struct{ requests []model.Request }

func (*fake) Info() model.Info { return model.Info{Name: "test"} }
func (p *fake) Stream(_ context.Context, r model.Request) iter.Seq2[model.Event, error] {
	return func(yield func(model.Event, error) bool) {
		p.requests = append(p.requests, r)
		m := model.Text(model.Assistant, "done")
		if len(p.requests) == 1 {
			m = model.Message{Role: model.Assistant, Content: []model.Part{{Type: "tool_call", Call: &model.ToolCall{ID: "call1", Name: "protected", Arguments: json.RawMessage(`{}`)}}}}
		}
		yield(model.Event{Type: model.Completed, Message: &m}, nil)
	}
}
func TestHostSettingsRefreshAndApproval(t *testing.T) {
	registry := tools.NewRegistry()
	called := false
	if err := registry.Register(tools.Tool{Name: "protected", Approval: mcp.ApprovalAsk, Handler: func(context.Context, tools.Call) (tools.Result, error) {
		called = true
		return tools.TextResult("executed"), nil
	}}); err != nil {
		t.Fatal(err)
	}
	history, err := contextwindow.New(model.Text(model.User, "hello"))
	if err != nil {
		t.Fatal(err)
	}
	p := &fake{}
	n := 0
	err = runLoop(context.Background(), p, registry, history, func() AppSettings {
		n++
		name := "first"
		if n > 1 {
			name = "updated"
		}
		return AppSettings{Model: config.Settings{Model: name}, MaxOutputTokens: 100, MaxSteps: 2}
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if called || len(p.requests) != 2 || p.requests[1].Settings.Model != "updated" {
		t.Fatalf("called=%v requests=%+v", called, p.requests)
	}
	result := p.requests[1].Messages[2].Content[0].Result
	if result == nil || !result.IsError {
		t.Fatalf("denial not returned: %+v", result)
	}
}
