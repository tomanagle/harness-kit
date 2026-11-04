package claude

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
)

func testImage(t *testing.T) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fixture.png")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, buf.Bytes()
}

func TestImagePromptUsesStreamJSONStdin(t *testing.T) {
	path, original := testImage(t)
	inputLog := filepath.Join(t.TempDir(), "stdin.json")
	rt, argLog := newFake(t, "2.1.317", "HK_FAKE_CLAUDE_INPUT="+inputLog)
	sess, err := rt.Start(context.Background(), agent.SessionConfig{Cwd: filepath.Dir(path)}, &recorder{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close(context.Background())
	res, err := sess.Prompt(context.Background(), agent.Turn{ID: "image", Input: []agent.InputPart{
		{Type: "text", Text: "before"},
		{Type: "image", Path: filepath.Base(path)},
		{Type: "text", Text: "after"},
	}})
	if err != nil || res.Status != event.TurnStatusCompleted {
		t.Fatalf("image turn: %+v %v", res, err)
	}
	args := invocations(t, argLog)[0]
	if got, _ := flag(args, "--input-format"); got != "stream-json" {
		t.Fatalf("image mode: %v", args)
	}
	if slices.Contains(args, path) || slices.Contains(args, "before") {
		t.Fatal("prompt data must not appear in argv")
	}
	raw, err := os.ReadFile(inputLog)
	if err != nil {
		t.Fatal(err)
	}
	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Source struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
				} `json:"source"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != "user" || msg.Message.Role != "user" || len(msg.Message.Content) != 3 {
		t.Fatalf("message: %s", raw)
	}
	blocks := msg.Message.Content
	if blocks[0].Text != "before" || blocks[2].Text != "after" || blocks[1].Type != "image" || blocks[1].Source.Type != "base64" || blocks[1].Source.MediaType != "image/png" {
		t.Fatalf("content order/types: %s", raw)
	}
	decoded, err := base64.StdEncoding.DecodeString(blocks[1].Source.Data)
	if err != nil || !bytes.Equal(decoded, original) {
		t.Fatalf("image bytes changed: %v", err)
	}
	if _, err := sess.Prompt(context.Background(), agent.Turn{ID: "text", Input: agent.Text("plain")}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(invocations(t, argLog)[1], "--input-format") {
		t.Fatal("image input mode leaked into the following text turn")
	}
}

func TestImageValidationAndTurnLimit(t *testing.T) {
	path, data := testImage(t)
	bad := filepath.Join(t.TempDir(), "not-an-image.png")
	if err := os.WriteFile(bad, []byte("plain text"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, in := range [][]agent.InputPart{
		{{Type: "image"}},
		{{Type: "image", Path: filepath.Dir(path)}},
		{{Type: "image", Path: bad}},
		{{Type: "unknown", Text: "hello"}},
	} {
		if _, _, err := encodeInput(context.Background(), osInputFiles{}, in, "", nil); !errors.Is(err, agent.ErrInvalidConfig) {
			t.Fatalf("invalid input %+v: %v", in, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := encodeInput(ctx, osInputFiles{}, []agent.InputPart{{Type: "image", Path: path}}, "", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled image input: %v", err)
	}
	zero := int64(0)
	rt, log := newFake(t, "2.1.317")
	negative := int64(-1)
	if err := rt.ValidateConfig(agent.SessionConfig{Options: Options{MaxImageBytes: &negative}}); !errors.Is(err, agent.ErrInvalidConfig) {
		t.Fatalf("negative cap: %v", err)
	}
	cfg := agent.SessionConfig{Model: "sonnet", Options: Options{MaxImageBytes: &zero}}
	sess, err := rt.Start(context.Background(), cfg, &recorder{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close(context.Background())
	turn := agent.Turn{ID: "blocked", Input: []agent.InputPart{{Type: "image", Path: path}}}
	if _, err := sess.Prompt(context.Background(), turn); !errors.Is(err, agent.ErrInvalidConfig) {
		t.Fatalf("zero cap should reject images: %v", err)
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected input launched subprocess: %v", err)
	}
	settings, err := cfg.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.Options["maxImageBytes"], _ = json.Marshal(len(data))
	prepared, err := sess.(agent.TurnPreparer).PrepareTurn(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	turn.ID, turn.Config = "allowed", &prepared
	if _, err := sess.Prompt(context.Background(), turn); err != nil {
		t.Fatalf("exact byte cap: %v", err)
	}
	turn.ID, turn.Config = "blocked-again", nil
	if _, err := sess.Prompt(context.Background(), turn); !errors.Is(err, agent.ErrInvalidConfig) {
		t.Fatalf("per-turn cap leaked into defaults: %v", err)
	}
}
