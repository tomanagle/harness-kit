package settings_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tomanagle/harness-kit/core/settings"
)

func TestFileStoreRoundTripAndCorruption(t *testing.T) {
	ctx := context.Background()
	store := settings.FileStore{Path: filepath.Join(t.TempDir(), "nested", "settings.json")}
	defaults := settings.Defaults()
	defaults.Logging.Level = "warn"
	got, err := settings.Load(ctx, store, defaults)
	if err != nil || got.Logging.Level != "warn" {
		t.Fatalf("missing: %+v %v", got, err)
	}
	if err = store.Save(ctx, got); err != nil {
		t.Fatal(err)
	}
	got, err = store.Load(ctx)
	if err != nil || got.Logging.Level != "warn" {
		t.Fatalf("load: %+v %v", got, err)
	}
	stat, _ := os.Stat(store.Path)
	if stat.Mode().Perm() != 0600 {
		t.Fatalf("mode=%v", stat.Mode())
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	got.Logging.Level = "debug"
	if err = store.Save(cancelled, got); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	saved, _ := store.Load(ctx)
	if saved.Logging.Level != "warn" {
		t.Fatal("cancelled save replaced prior document")
	}
	if err = os.WriteFile(store.Path, []byte(`{"bogus":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = settings.Load(ctx, store, defaults); err == nil {
		t.Fatal("corruption silently replaced with defaults")
	}
}
