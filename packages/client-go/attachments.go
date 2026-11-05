package harnesskit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// AttachmentFiles is an injectable local-file boundary. Implementations must
// reject non-regular files and return their size before reading contents.
type AttachmentFiles interface {
	OpenAttachment(context.Context, string) (io.ReadCloser, int64, string, error)
}
type LocalAttachmentFiles struct{}

func (LocalAttachmentFiles) OpenAttachment(ctx context.Context, path string) (io.ReadCloser, int64, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, "", err
	}
	if !filepath.IsAbs(path) {
		return nil, 0, "", fmt.Errorf("harness-kit: attachment path must be absolute")
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, 0, "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, "", err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, "", fmt.Errorf("harness-kit: %s is not a regular file", path)
	}
	file, err := openAttachment(path)
	if err != nil {
		return nil, 0, "", err
	}
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, 0, "", fmt.Errorf("harness-kit: attachment changed while opening")
	}
	return file, info.Size(), path, nil
}

type AttachmentOptions struct {
	Files AttachmentFiles
	// Nil selects defaults. Zero is an explicit zero-byte/file limit.
	MaxFiles                                                           *int
	MaxTextBytes, MaxImageBytes, MaxTotalTextBytes, MaxTotalImageBytes *int64
}
type Attachment struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Size int64  `json:"size"`
}
type PreparedAttachments struct {
	Input       []InputPart  `json:"input"`
	Attachments []Attachment `json:"attachments"`
}

func attachmentImage(header []byte) bool {
	return bytes.HasPrefix(header, []byte{137, 80, 78, 71, 13, 10, 26, 10}) || bytes.HasPrefix(header, []byte{255, 216, 255}) || bytes.HasPrefix(header, []byte("GIF87a")) || bytes.HasPrefix(header, []byte("GIF89a")) || (len(header) >= 12 && string(header[:4]) == "RIFF" && string(header[8:12]) == "WEBP")
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// PrepareAttachments inlines UTF-8 text and passes local image references.
// The runtime must share the selected filesystem. It never sends a model request.
func PrepareAttachments(ctx context.Context, prompt string, paths []string, options AttachmentOptions) (PreparedAttachments, error) {
	out := PreparedAttachments{Input: []InputPart{{Type: "text", Text: prompt}}, Attachments: []Attachment{}}
	value := func(p *int64, def int64) int64 {
		if p != nil {
			return *p
		}
		return def
	}
	maxFiles := 20
	if options.MaxFiles != nil {
		maxFiles = *options.MaxFiles
	}
	textLimit, imageLimit := value(options.MaxTextBytes, 512*1024), value(options.MaxImageBytes, 5*1024*1024)
	totalText, totalImage := value(options.MaxTotalTextBytes, 1024*1024), value(options.MaxTotalImageBytes, 20*1024*1024)
	if maxFiles < 0 || textLimit < 0 || imageLimit < 0 || totalText < 0 || totalImage < 0 {
		return out, fmt.Errorf("harness-kit: attachment limits cannot be negative")
	}
	if len(paths) > maxFiles {
		return out, fmt.Errorf("harness-kit: choose up to %d files", maxFiles)
	}
	files := options.Files
	if files == nil {
		files = LocalAttachmentFiles{}
	}
	seen := map[string]bool{}
	var textBytes, imageBytes int64
	for _, path := range paths {
		reader, size, canonical, err := files.OpenAttachment(ctx, path)
		if err != nil {
			return out, err
		}
		if reader == nil || size < 0 || canonical == "" {
			if reader != nil {
				_ = reader.Close()
			}
			return out, fmt.Errorf("harness-kit: invalid attachment file metadata")
		}
		if seen[canonical] {
			_ = reader.Close()
			continue
		}
		seen[canonical] = true
		header := make([]byte, 12)
		n, err := io.ReadFull(contextReader{ctx, reader}, header)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			_ = reader.Close()
			return out, err
		}
		header = header[:n]
		kind := "text"
		limit := textLimit
		if attachmentImage(header) {
			kind = "image"
			limit = imageLimit
		}
		if size > limit {
			_ = reader.Close()
			return out, fmt.Errorf("harness-kit: %s exceeds its attachment byte limit", canonical)
		}
		remaining := size - int64(n)
		if remaining < 0 || remaining == int64(^uint64(0)>>1) {
			_ = reader.Close()
			return out, fmt.Errorf("harness-kit: attachment grew beyond its limit")
		}
		// Bound each read even if the file grows after stat.
		rest, err := io.ReadAll(io.LimitReader(contextReader{ctx, reader}, remaining+1))
		_ = reader.Close()
		if err != nil {
			return out, err
		}
		data := append(header, rest...)
		size = int64(len(data))
		if int64(len(rest)) > remaining {
			return out, fmt.Errorf("harness-kit: attachment grew beyond its limit")
		}
		if kind == "text" {
			if !utf8.Valid(data) || bytes.ContainsRune(data, 0) {
				return out, fmt.Errorf("harness-kit: %s is not UTF-8 text or a supported image", canonical)
			}
			textBytes += size
		} else {
			imageBytes += size
		}
		if textBytes > totalText || imageBytes > totalImage {
			return out, fmt.Errorf("harness-kit: attachments exceed total byte limits")
		}
		out.Attachments = append(out.Attachments, Attachment{Path: canonical, Name: filepath.Base(canonical), Kind: kind, Size: size})
		if kind == "image" {
			out.Input = append(out.Input, InputPart{Type: "image", Path: canonical})
		} else {
			quoted, _ := json.Marshal(canonical)
			out.Input = append(out.Input, InputPart{Type: "text", Text: fmt.Sprintf("Attached file %s (context):\n%s", quoted, data)})
		}
	}
	return out, nil
}
