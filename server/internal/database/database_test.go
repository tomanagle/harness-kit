package database

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestOpenReturnsStartupErrorsWithoutLeakingURL(t *testing.T) {
	cfg := Defaults()
	cfg.URL = "postgres://private-password:%zz@invalid/"
	_, err := Open(context.Background(), cfg, Runtime)
	if err == nil || strings.Contains(err.Error(), "private-password") {
		t.Fatalf("unsafe parse error: %v", err)
	}
	cfg.URL = "postgres://localhost/harness"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = Open(ctx, cfg, Runtime); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancelled startup, got %v", err)
	}
}
