package contextwindow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/tomanagle/harness-kit/config"
	"github.com/tomanagle/harness-kit/model"
)

func fixture() model.Request {
	return model.Request{Settings: config.Settings{Model: "chosen-model"}, MaxOutputTokens: 2, Messages: []model.Message{
		model.Text(model.System, "keep instructions"), model.Text(model.User, "old question"),
		{Role: model.Assistant, Content: []model.Part{{Type: "tool_call", Call: &model.ToolCall{ID: "a", Name: "test", Arguments: json.RawMessage(`{}`)}}}},
		model.Result("a", "done", false), model.Text(model.Assistant, "old answer"), model.Text(model.User, "new question"),
	}}
}
func countMessages(_ context.Context, r model.Request) (Estimate, error) {
	return Estimate{Tokens: int64(len(r.Messages)), Exact: true}, nil
}

func TestTrimCompleteExchangeAndReserveOutput(t *testing.T) {
	r := fixture()
	out, err := Fit(context.Background(), r, Config{MaxTokens: 5}, countMessages, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Request.Messages) != 2 || out.Request.Messages[0].Role != model.System || out.Request.Messages[1].Text() != "new question" || out.DroppedMessages != 4 {
		t.Fatalf("%+v", out)
	}
	if !out.After.Exact || len(r.Messages) != 6 {
		t.Fatal("wrong estimate or mutated original")
	}
	_, err = Fit(context.Background(), r, Config{MaxTokens: 3}, countMessages, nil)
	if !errors.Is(err, ErrBudget) {
		t.Fatalf("protected latest turn exceeded budget: %v", err)
	}
}
func TestCompactorCannotMutateProtectedInput(t *testing.T) {
	r := fixture()
	_, err := Fit(context.Background(), r, Config{MaxTokens: 5}, countMessages, func(_ context.Context, req model.Request, _ int64) ([]model.Message, error) {
		req.Messages[0].Content[0].Text = "changed"
		return []model.Message{req.Messages[0], req.Messages[5]}, nil
	})
	if err == nil || r.Messages[0].Text() != "keep instructions" {
		t.Fatalf("protected mutation accepted: %v", err)
	}
	out, err := Fit(context.Background(), r, Config{MaxTokens: 6}, countMessages, func(_ context.Context, req model.Request, _ int64) ([]model.Message, error) {
		return []model.Message{req.Messages[0], model.Text(model.User, "summary"), req.Messages[5]}, nil
	})
	if err != nil || !out.Compacted {
		t.Fatalf("valid compaction: %v", err)
	}
}
func TestSnapshotRestoreAndConflict(t *testing.T) {
	w, err := New(model.Text(model.User, "first"))
	if err != nil {
		t.Fatal(err)
	}
	s := w.Snapshot()
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	s.Messages[0].Content[0].Text = "changed"
	if w.Snapshot().Messages[0].Text() != "first" {
		t.Fatal("snapshot aliases history")
	}
	var decoded Snapshot
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	restored, err := Restore(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if err = restored.Append(model.Text(model.Assistant, "answer")); err != nil {
		t.Fatal(err)
	}
	if err = restored.Replace(decoded.Revision, decoded.Messages); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err = w.Replace(s.Revision, decoded.Messages); err != nil {
		t.Fatal(err)
	}
}
func TestApproximateAndCancellation(t *testing.T) {
	r := fixture()
	v, err := ApproximateJSON(4)(context.Background(), r)
	if err != nil || v.Exact || v.Tokens <= 0 {
		t.Fatalf("%+v %v", v, err)
	}
	if _, err = ApproximateJSON(0)(context.Background(), r); err == nil {
		t.Fatal("invalid ratio accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = Fit(ctx, r, Config{}, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestPreparedSchemasAndSettingsAreIndependent(t *testing.T) {
	r := fixture()
	r.Tools = []model.Tool{{Name: "test", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	r.Settings.Options = map[string]json.RawMessage{"test": json.RawMessage(`true`)}
	out, err := Fit(context.Background(), r, Config{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out.Request.Tools[0].Name = "changed"
	out.Request.Tools[0].InputSchema[0] = 'x'
	out.Request.Settings.Options["test"][0] = 'x'
	if r.Tools[0].Name != "test" || !json.Valid(r.Tools[0].InputSchema) || !json.Valid(r.Settings.Options["test"]) {
		t.Fatal("prepared request aliases original")
	}
}
