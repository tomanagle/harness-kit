package openai

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

	"github.com/openai/openai-go/v3/option"
	"github.com/tomanagle/harness-kit/core/config"
	"github.com/tomanagle/harness-kit/core/model"
)

func emit(w http.ResponseWriter, data string) { fmt.Fprintf(w, "data: %s\n\n", data) }
func request() model.Request {
	return model.Request{Settings: config.Settings{Model: "caller-model", Effort: "low", Options: map[string]json.RawMessage{"store": json.RawMessage(`false`)}}, Messages: []model.Message{model.Text(model.User, "hello")}, MaxOutputTokens: 128}
}

func TestStreamingNativeContinuation(t *testing.T) {
	var received map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("request: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit(w, `{"type":"response.reasoning_summary_text.delta","delta":"planning"}`)
		emit(w, `{"type":"response.output_text.delta","delta":"hello"}`)
		emit(w, `{"type":"response.output_item.added","item":{"type":"function_call","id":"item1","call_id":"call1","name":"lookup","arguments":""}}`)
		emit(w, `{"type":"response.function_call_arguments.delta","item_id":"item1","delta":"{\"key\":"}`)
		emit(w, `{"type":"response.function_call_arguments.delta","item_id":"item1","delta":"\"x\"}"}`)
		emit(w, `{"type":"response.output_item.done","item":{"type":"function_call","id":"item1","call_id":"call1","name":"lookup","arguments":"{\"key\":\"x\"}"}}`)
		emit(w, `{"type":"response.completed","response":{"id":"resp1","status":"completed","output":[{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"planning"}],"encrypted_content":"opaque-signed-state"},{"type":"message","id":"m1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[]}]},{"type":"function_call","id":"item1","call_id":"call1","name":"lookup","arguments":"{\"key\":\"x\"}","status":"completed"}],"usage":{"input_tokens":20,"output_tokens":7,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":2}}}}`)
	}))
	defer server.Close()
	p := New(option.WithBaseURL(server.URL), option.WithAPIKey("test-key"))
	r := request()
	r.Tools = []model.Tool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object","properties":{"key":{"type":"string"}}}`)}}
	r.Messages[0].Content = append(r.Messages[0].Content, model.Part{Type: "image", Image: &model.Image{Data: "eA==", MediaType: "image/png"}})
	var kinds []model.EventType
	m, u, err := model.Collect(context.Background(), p, r, func(e model.Event) bool { kinds = append(kinds, e.Type); return true })
	if err != nil {
		t.Fatal(err)
	}
	if m.Text() != "hello" || len(m.Calls()) != 1 || string(m.Calls()[0].Arguments) != `{"key":"x"}` || u.InputTokens != 20 || u.CacheReadTokens != 4 || u.ReasoningTokens != 2 {
		t.Fatalf("message=%+v usage=%+v", m, u)
	}
	if len(kinds) != 7 || kinds[0] != model.ReasoningDelta || kinds[4] != model.ToolCallComplete {
		t.Fatalf("events: %v", kinds)
	}
	if string(received["model"]) != `"caller-model"` || string(received["stream"]) != "true" || string(received["store"]) != "false" || !strings.Contains(string(received["input"]), "data:image/png;base64,eA==") {
		t.Fatalf("body: %s", received)
	}
	r.Messages = append(r.Messages, m, model.Result("call1", "found", false))
	body, err := requestBody(r)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(body)
	if !strings.Contains(string(data), "opaque-signed-state") || !strings.Contains(string(data), "function_call_output") {
		t.Fatalf("continuation lost state: %s", data)
	}
}

func TestFailuresAndOverrides(t *testing.T) {
	for name, events := range map[string]string{"eof": "", "bad_tool": `{"type":"response.output_item.done","item":{"type":"function_call","call_id":"x","name":"lookup","arguments":"{"}}`, "error": `{"type":"error","message":"upstream failed"}`} {
		t.Run(name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if events != "" {
					emit(w, events)
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
	r.Settings.Options["model"] = json.RawMessage(`"override"`)
	if _, err := requestBody(r); !errors.Is(err, model.ErrInvalid) {
		t.Fatal(err)
	}
	r = request()
	r.Messages[0].Native = &model.Native{Provider: "anthropic", Data: json.RawMessage(`[]`)}
	if _, err := requestBody(r); !errors.Is(err, model.ErrUnsupported) {
		t.Fatal(err)
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"bad key","type":"authentication_error"}}`)
	}))
	defer s.Close()
	if _, _, err := model.Collect(context.Background(), New(option.WithBaseURL(s.URL), option.WithAPIKey("test")), request(), nil); err == nil {
		t.Fatal("HTTP error ignored")
	}
}

func TestEarlyStopClosesHTTPStream(t *testing.T) {
	closed := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		emit(w, `{"type":"response.output_text.delta","delta":"one"}`)
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
