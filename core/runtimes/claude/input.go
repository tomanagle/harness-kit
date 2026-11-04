package claude

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"

	"github.com/tomanagle/harness-kit/core/agent"
)

// encodeInput uses Claude's documented user-message content format for image
// prompts. Plain text retains the existing input mode. Neither data nor prompt
// text appears in argv; the complete message is written to the child's stdin.
func encodeInput(ctx context.Context, input []agent.InputPart, cwd string, maxImageBytes *int64) ([]byte, bool, error) {
	images := false
	for _, p := range input {
		switch p.Type {
		case "", "text":
		case "image":
			images = true
		default:
			return nil, false, fmt.Errorf("%w: claude: unsupported input type %q", agent.ErrInvalidConfig, p.Type)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if !images {
		return []byte(agent.JoinText(input)), false, nil
	}
	blocks := make([]any, 0, len(input))
	for _, p := range input {
		if p.Type != "image" {
			blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
			continue
		}
		if p.Path == "" {
			return nil, false, fmt.Errorf("%w: claude: image path is required", agent.ErrInvalidConfig)
		}
		path := p.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		data, err := readImage(ctx, path, maxImageBytes)
		if err != nil {
			return nil, false, err
		}
		media := http.DetectContentType(data)
		switch media {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
		default:
			return nil, false, fmt.Errorf("%w: claude: unsupported image media type %s", agent.ErrInvalidConfig, media)
		}
		blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": media, "data": base64.StdEncoding.EncodeToString(data)}})
	}
	message := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": blocks}, "parent_tool_use_id": nil}
	b, err := json.Marshal(message)
	return append(b, '\n'), true, err
}

func readImage(ctx context.Context, path string, maximum *int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("claude: open image: %w", err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: claude: image must be a regular file", agent.ErrInvalidConfig)
	}
	if maximum != nil && (*maximum < 0 || stat.Size() > *maximum) {
		return nil, fmt.Errorf("%w: claude: image exceeds maxImageBytes", agent.ErrInvalidConfig)
	}
	var reader io.Reader = f
	if maximum != nil && *maximum < math.MaxInt64 {
		reader = io.LimitReader(f, *maximum+1)
	}
	b, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maximum != nil && int64(len(b)) > *maximum {
		return nil, fmt.Errorf("%w: claude: image exceeds maxImageBytes", agent.ErrInvalidConfig)
	}
	return b, nil
}
