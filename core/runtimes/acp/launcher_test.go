package acp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/internal/processtest"
	"github.com/tomanagle/harness-kit/core/runtimes/transport"
)

func TestInjectedLauncherHandlesDiscoveryAndSession(t *testing.T) {
	launcher := &processtest.Launcher{Run: func(_ context.Context, command transport.Command, input io.Reader, output io.Writer) error {
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
			case "initialize":
				result = map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "agentInfo": map[string]any{"name": "virtual", "version": "1.2.3"}}
			case "session/new":
				result = map[string]any{"sessionId": "virtual-session"}
			}
			if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": *request.ID, "result": result}); err != nil {
				return err
			}
		}
	}}
	runtime := New("virtual-agent", WithLauncher(launcher), WithEnv(map[string]string{"VIRTUAL": "1"}))
	discovered, err := runtime.Discover(context.Background())
	if err != nil || !discovered.Ready || discovered.Version != "1.2.3" {
		t.Fatalf("discover: %+v %v", discovered, err)
	}
	live, err := runtime.Start(context.Background(), agent.SessionConfig{}, &captureHandler{})
	if err != nil {
		t.Fatal(err)
	}
	if live.RuntimeSessionID() != "virtual-session" {
		t.Fatal(live.RuntimeSessionID())
	}
	if err := live.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	commands := launcher.Commands()
	if len(commands) != 2 {
		t.Fatalf("discovery/session: %+v", commands)
	}
	for _, command := range commands {
		if len(command.Env) != 1 || command.Env[0] != "VIRTUAL=1" {
			t.Fatal(command.Env)
		}
	}
}

type virtualInputFiles struct{ fstest.MapFS }

func (f virtualInputFiles) Open(name string) (fs.File, error) {
	// InputFileOpener receives native paths; MapFS requires slash-separated names.
	return f.MapFS.Open(filepath.ToSlash(name))
}

func TestInjectedImageFilesRetainSizeAndTypeChecks(t *testing.T) {
	image := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 64))
	files := fstest.MapFS{"virtual/image.png": &fstest.MapFile{Data: image}, "virtual/text.png": &fstest.MapFile{Data: []byte("not an image")}}
	runtime := New("virtual-agent", WithInputFiles(virtualInputFiles{files}))
	session := &Session{rt: runtime, cwd: "virtual"}
	session.init.AgentCapabilities.Prompt.Image = true
	input := []agent.InputPart{{Type: "image", Path: "image.png"}}
	content, err := session.content(input)
	if err != nil || content[0].(map[string]any)["data"] != base64.StdEncoding.EncodeToString(image) {
		t.Fatalf("injected image: %v %v", content, err)
	}
	runtime.maxImageBytes = int64(len(image) - 1)
	if _, err := session.content(input); !errors.Is(err, agent.ErrInvalidConfig) {
		t.Fatalf("size limit: %v", err)
	}
	runtime.maxImageBytes = 1024
	if _, err := session.content([]agent.InputPart{{Type: "image", Path: "text.png"}}); !errors.Is(err, agent.ErrInvalidConfig) {
		t.Fatalf("media check: %v", err)
	}
}
