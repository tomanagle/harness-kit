// Package logger constructs the process logger. Consumers receive *slog.Logger;
// slog.Handler remains the standard interface for custom sinks and formatting.
package logger

import (
	"errors"
	"io"
	"log/slog"
)

type Config struct{ Level slog.Level }

func New(output io.Writer, cfg Config) (*slog.Logger, error) {
	if output == nil {
		return nil, errors.New("logger: output is required")
	}
	return slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: cfg.Level})), nil
}

// MustNew is the fail-fast startup variant of New. Call it only from main.
func MustNew(output io.Writer, cfg Config) *slog.Logger {
	logger, err := New(output, cfg)
	if err != nil {
		panic(err)
	}
	return logger
}
