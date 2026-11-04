package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/mcp"
)

type ticketIn struct {
	Title    string            `json:"title" desc:"Short summary"`
	Priority string            `json:"priority,omitempty" enum:"low,high"`
	Tags     []string          `json:"tags,omitempty"`
	Count    int               `json:"count,omitempty"`
	Owner    *string           `json:"owner"`
	Extra    map[string]string `json:"extra,omitempty"`
	When     time.Time         `json:"when,omitempty"`
	secret   string
}

type ticketOut struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

func TestSchemaInference(t *testing.T) {
	b, err := SchemaOf(ticketIn{})
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Type       string                    `json:"type"`
		Required   []string                  `json:"required"`
		Properties map[string]map[string]any `json:"properties"`
	}
	json.Unmarshal(b, &s)
	if s.Type != "object" || len(s.Required) != 1 || s.Required[0] != "title" {
		t.Fatalf("required: %s", b)
	}
	p := s.Properties
	if p["title"]["description"] != "Short summary" || p["priority"]["enum"].([]any)[1] != "high" || p["tags"]["type"] != "array" ||
		p["count"]["type"] != "integer" || p["extra"]["type"] != "object" || p["when"]["format"] != "date-time" {
		t.Fatalf("properties: %s", b)
	}
	if _, ok := p["secret"]; ok {
		t.Fatal("unexported fields must be skipped")
	}
	if _, err := SchemaOf(42); err == nil {
		t.Fatal("non-struct arguments should be rejected")
	}
}

func TestValidate(t *testing.T) {
	schema, _ := SchemaOf(ticketIn{})
	ok := []string{
		`{"title": "x"}`,
		`{"title": "x", "priority": "low", "tags": ["a"], "count": 3, "owner": null}`,
	}
	for _, v := range ok {
		if err := Validate(schema, json.RawMessage(v)); err != nil {
			t.Errorf("%s: %v", v, err)
		}
	}
	bad := map[string]string{
		`{}`:                              "title: required",
		`{"title": 1}`:                    "expected string",
		`{"title": "x", "priority": "z"}`: "one of",
		`{"title": "x", "tags": [1]}`:     "tags[0]",
		`{"title": "x", "count": 1.5}`:    "expected integer",
		`[1]`:                             "expected object",
	}
	for v, want := range bad {
		if err := Validate(schema, json.RawMessage(v)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", v, err, want)
		}
	}
	strict := json.RawMessage(`{"type":"object","properties":{"a":{"type":"string","minLength":2}},"additionalProperties":false}`)
	if err := Validate(strict, json.RawMessage(`{"b":1}`)); err == nil {
		t.Fatal("additionalProperties false")
	}
	if err := Validate(strict, json.RawMessage(`{"a":"x"}`)); err == nil {
		t.Fatal("minLength")
	}
}

func TestRegistryAndCall(t *testing.T) {
	r := NewRegistry()
	changes := 0
	stop := r.OnChange(func() { changes++ })
	defer stop()

	err := Add(r, "create_ticket", "Create a ticket", func(ctx context.Context, in ticketIn) (ticketOut, error) {
		call, _ := CallFromContext(ctx)
		if in.Title == "boom" {
			return ticketOut{}, errors.New("backend down")
		}
		if in.Title == "panic" {
			panic("oops")
		}
		return ticketOut{ID: "T-1 " + call.SessionID, Title: in.Title}, nil
	}, WithApproval(mcp.ApprovalAsk))
	if err != nil {
		t.Fatal(err)
	}
	if err := Add(r, "create_ticket", "", func(context.Context, ticketIn) (string, error) { return "", nil }); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if tl, _ := r.Get("create_ticket"); tl.Approval != mcp.ApprovalAsk {
		t.Fatal("option not applied")
	}

	ctx := context.Background()
	res, err := r.Call(ctx, Call{SessionID: "s1", Name: "create_ticket", Arguments: json.RawMessage(`{"title":"hello"}`)})
	if err != nil || res.IsError || !strings.Contains(res.Text(), `"id":"T-1 s1"`) || res.Structured == nil {
		t.Fatalf("call: %+v %v", res, err)
	}
	for args, want := range map[string]string{
		`{"title":"boom"}`:  "backend down",
		`{"title":"panic"}`: "panicked",
		`{"nope":1}`:        "invalid arguments",
	} {
		res, err := r.Call(ctx, Call{Name: "create_ticket", Arguments: json.RawMessage(args)})
		if err != nil || !res.IsError || !strings.Contains(res.Text(), want) {
			t.Errorf("%s: %+v %v", args, res, err)
		}
	}
	if _, err := r.Call(ctx, Call{Name: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}

	// String outputs stay plain text; timeouts become error results.
	Add(r, "slow", "", func(ctx context.Context, _ struct{}) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}, WithTimeout(20*time.Millisecond))
	Add(r, "hi", "", func(context.Context, struct{}) (string, error) { return "hello", nil }, ReadOnly())
	if res, _ := r.Call(ctx, Call{Name: "hi"}); res.Text() != "hello" || res.Structured != nil {
		t.Fatalf("string result: %+v", res)
	}
	if res, _ := r.Call(ctx, Call{Name: "slow"}); !res.IsError || !strings.Contains(res.Text(), "timed out") {
		t.Fatalf("timeout: %+v", res)
	}
	r.Unregister("hi", "unknown")
	if _, ok := r.Get("hi"); ok || len(r.List()) != 2 || changes != 4 {
		t.Fatalf("unregister: %d tools, %d changes", len(r.List()), changes)
	}
	if err := r.Register(Tool{Name: "bad name", Handler: func(context.Context, Call) (Result, error) { return Result{}, nil }}); err == nil {
		t.Fatal("invalid name accepted")
	}
	if err := r.Register(Tool{Name: "noschema"}); err == nil {
		t.Fatal("missing handler accepted")
	}
}

func TestMaxConcurrent(t *testing.T) {
	r := NewRegistry(MaxConcurrent(1))
	var running, peak atomic.Int32
	Add(r, "work", "", func(context.Context, struct{}) (string, error) {
		n := running.Add(1)
		if n > peak.Load() {
			peak.Store(n)
		}
		time.Sleep(10 * time.Millisecond)
		running.Add(-1)
		return "ok", nil
	})
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func() { r.Call(context.Background(), Call{Name: "work"}); done <- struct{}{} }()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	if peak.Load() != 1 {
		t.Fatalf("peak concurrency %d", peak.Load())
	}
}
