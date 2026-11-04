package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/tomanagle/harness-kit/core/config"
	"github.com/tomanagle/harness-kit/core/model"
)

func emit(w http.ResponseWriter, data string) {
	var v struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(data), &v)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", v.Type, data)
}
func request() model.Request {
	return model.Request{Settings: config.Settings{Model: "caller-model", Effort: "low", Options: map[string]json.RawMessage{"temperature": json.RawMessage(`0`)}}, MaxOutputTokens: 128, Messages: []model.Message{model.Text(model.System, "instructions"), model.Text(model.User, "hello")}}
}

const start = `{"type":"message_start","message":{"id":"msg1","type":"message","role":"assistant","model":"caller-model","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0,"cache_read_input_tokens":4,"cache_creation_input_tokens":2}}}`

func TestStreamingSignedContinuation(t *testing.T) {
	var received map[string]json.RawMessage
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != "test-key" {
			t.Errorf("request path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, data := range []string{start,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"planning"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"signed-state"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hello"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call1","name":"lookup","input":{}}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"key\":"}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"x\"}"}}`,
			`{"type":"content_block_stop","index":2}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":7}}`,
			`{"type":"message_stop"}`,
		} {
			emit(w, data)
		}
	}))
	defer s.Close()
	r := request()
	r.Tools = []model.Tool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	r.Messages[1].Content = append(r.Messages[1].Content, model.Part{Type: "image", Image: &model.Image{URL: "https://example.com/image.png"}})
	var kinds []model.EventType
	m, u, err := model.Collect(context.Background(), New(option.WithBaseURL(s.URL), option.WithAPIKey("test-key")), r, func(e model.Event) bool { kinds = append(kinds, e.Type); return true })
	if err != nil {
		t.Fatal(err)
	}
	if m.Text() != "hello" || len(m.Calls()) != 1 || string(m.Calls()[0].Arguments) != `{"key":"x"}` || u.InputTokens != 16 || u.CacheReadTokens != 4 || u.CacheWriteTokens != 2 || u.OutputTokens != 7 {
		t.Fatalf("message=%+v usage=%+v", m, u)
	}
	if len(kinds) != 8 || kinds[1] != model.ReasoningDelta || kinds[5] != model.ToolCallComplete {
		t.Fatalf("events %v", kinds)
	}
	if string(received["model"]) != `"caller-model"` || string(received["stream"]) != "true" || string(received["temperature"]) != "0" || !strings.Contains(string(received["messages"]), "https://example.com/image.png") {
		t.Fatalf("body: %s", received)
	}
	r.Messages = append(r.Messages, m, model.Result("call1", "found", false))
	body, err := requestBody(r)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(body)
	if !strings.Contains(string(data), "signed-state") || !strings.Contains(string(data), "tool_result") {
		t.Fatalf("lost state: %s", data)
	}
}

func TestFailuresAndOverrides(t *testing.T) {
	for name, events := range map[string][]string{"eof": {start}, "bad_tool": {start, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"x","name":"lookup","input":{}}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{"}}`, `{"type":"content_block_stop","index":0}`}, "error": {`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`}} {
		t.Run(name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, data := range events {
					emit(w, data)
				}
			}))
			defer s.Close()
			_, _, err := model.Collect(context.Background(), New(option.WithBaseURL(s.URL), option.WithAPIKey("test")), request(), nil)
			if err == nil {
				t.Fatal("accepted bad stream")
			}
			if name == "eof" && !errors.Is(err, model.ErrIncomplete) {
				t.Fatal(err)
			}
		})
	}
	r := request()
	r.MaxOutputTokens = 0
	if _, err := requestBody(r); !errors.Is(err, model.ErrInvalid) {
		t.Fatal(err)
	}
	r = request()
	r.Settings.Options["output_config"] = json.RawMessage(`{}`)
	if _, err := requestBody(r); !errors.Is(err, model.ErrInvalid) {
		t.Fatal(err)
	}
	r = request()
	r.Messages[1].Native = &model.Native{Provider: "openai", Data: json.RawMessage(`[]`)}
	if _, err := requestBody(r); !errors.Is(err, model.ErrUnsupported) {
		t.Fatal(err)
	}
}

func TestEarlyStopClosesHTTPStream(t *testing.T) {
	closed := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		emit(w, start)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(closed)
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := model.Collect(ctx, New(option.WithBaseURL(s.URL), option.WithAPIKey("test")), request(), func(model.Event) bool { return false })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("stream stayed open after iterator stopped")
	}
}
