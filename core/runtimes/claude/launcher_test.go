package claude

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/internal/processtest"
	"github.com/tomanagle/harness-kit/core/runtimes/transport"
)

func TestInjectedLauncherHandlesProbesAndPrompt(t *testing.T) {
	launcher := &processtest.Launcher{Run: func(_ context.Context, command transport.Command, input io.Reader, output io.Writer) error {
		switch command.Args[0] {
		case "--version":
			_, err := fmt.Fprintln(output, "claude 2.1.317")
			return err
		case "auth":
			_, err := fmt.Fprintln(output, `{"loggedIn":true,"authMethod":"api_key"}`)
			return err
		}
		prompt, err := io.ReadAll(input)
		if err != nil {
			return err
		}
		if string(prompt) != "virtual input" {
			return fmt.Errorf("wrong stdin: %s", prompt)
		}
		_, err = fmt.Fprintln(output, `{"type":"result","subtype":"success","is_error":false,"session_id":"virtual-session","result":"virtual output"}`)
		return err
	}}
	runtime := New(WithLauncher(launcher), WithEnv("VIRTUAL=1"))
	discovered, err := runtime.Discover(context.Background())
	if err != nil || !discovered.Ready || discovered.Version != "2.1.317" {
		t.Fatalf("discover: %+v %v", discovered, err)
	}
	live, err := runtime.Start(context.Background(), agent.SessionConfig{}, &recorder{})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close(context.Background())
	result, err := live.Prompt(context.Background(), agent.Turn{ID: "turn", Input: agent.Text("virtual input")})
	if err != nil || result.Status != event.TurnStatusCompleted {
		t.Fatalf("prompt: %+v %v", result, err)
	}
	commands := launcher.Commands()
	if len(commands) != 3 {
		t.Fatalf("version/auth/prompt: %+v", commands)
	}
	for _, command := range commands {
		if len(command.Env) != 1 || command.Env[0] != "VIRTUAL=1" || strings.Contains(strings.Join(command.Args, " "), "virtual input") {
			t.Fatalf("command: %+v", command)
		}
	}
}

func TestInjectedImageFilesRetainSizeAndTypeChecks(t *testing.T) {
	image := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 64))
	files := fstest.MapFS{"virtual/image.png": &fstest.MapFile{Data: image}, "virtual/text.png": &fstest.MapFile{Data: []byte("not an image")}}
	runtime := New(WithInputFiles(files))
	input := []agent.InputPart{{Type: "image", Path: "image.png"}}
	data, streaming, err := encodeInput(context.Background(), runtime.inputFiles, input, "virtual", nil)
	if err != nil || !streaming || !strings.Contains(string(data), base64.StdEncoding.EncodeToString(image)) {
		t.Fatalf("injected image: %s %v %v", data, streaming, err)
	}
	limit := int64(len(image) - 1)
	if _, _, err := encodeInput(context.Background(), runtime.inputFiles, input, "virtual", &limit); !errors.Is(err, agent.ErrInvalidConfig) {
		t.Fatalf("size limit: %v", err)
	}
	if _, _, err := encodeInput(context.Background(), runtime.inputFiles, []agent.InputPart{{Type: "image", Path: "text.png"}}, "virtual", nil); !errors.Is(err, agent.ErrInvalidConfig) {
		t.Fatalf("media check: %v", err)
	}
}
