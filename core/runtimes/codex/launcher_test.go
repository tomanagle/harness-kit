package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/internal/processtest"
	"github.com/tomanagle/harness-kit/core/runtimes/transport"
)

func TestInjectedLauncherHandlesDiscoveryAndSession(t *testing.T) {
	launcher := &processtest.Launcher{Run: func(_ context.Context, command transport.Command, input io.Reader, output io.Writer) error {
		if command.Args[0] == "--version" {
			_, err := fmt.Fprintln(output, "codex 0.166.0")
			return err
		}
		decoder, encoder := json.NewDecoder(input), json.NewEncoder(output)
		for {
			var request struct {
				ID     *int   `json:"id"`
				Method string `json:"method"`
			}
			if err := decoder.Decode(&request); err != nil {
				return nil
			}
			if request.ID == nil {
				continue
			}
			var result any = map[string]any{}
			switch request.Method {
			case "account/read":
				result = map[string]any{"account": map[string]any{"type": "apiKey"}}
			case "thread/start":
				result = map[string]any{"thread": map[string]any{"id": "virtual-thread"}, "model": "virtual-model"}
			case "model/list":
				result = map[string]any{"data": []any{}}
			}
			if err := encoder.Encode(map[string]any{"id": *request.ID, "result": result}); err != nil {
				return err
			}
		}
	}}
	runtime := New(WithLauncher(launcher), WithEnv("VIRTUAL=1"))
	discovered, err := runtime.Discover(context.Background())
	if err != nil || !discovered.Ready || discovered.Version != "0.166.0" {
		t.Fatalf("discover: %+v %v", discovered, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	live, err := runtime.Start(ctx, agent.SessionConfig{}, &recorder{})
	if err != nil {
		t.Fatal(err)
	}
	cancel() // Startup cancellation must not terminate a successfully returned child.
	if live.RuntimeSessionID() != "virtual-thread" {
		t.Fatal(live.RuntimeSessionID())
	}
	if err := live.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	commands := launcher.Commands()
	if len(commands) != 3 {
		t.Fatalf("discovery/version/session commands: %+v", commands)
	}
	for _, command := range commands {
		if len(command.Env) != 1 || command.Env[0] != "VIRTUAL=1" {
			t.Fatal(command.Env)
		}
	}
}
