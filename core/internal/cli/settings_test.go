package cli

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/tomanagle/harness-kit/core/settings"
)

func TestStartupSettingsFileAndExplicitFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"process":{"runtimes":["fake"],"store":"saved-store"},"logging":{"level":"debug"},"memory":{"maxCacheBytes":0,"evictAfterIdleMs":600000},"session":{"model":"from-settings"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, override := range []bool{false, true} {
		flags := flag.NewFlagSet("test", flag.ContinueOnError)
		flags.String("store", "memory", "")
		flags.String("log-level", "info", "")
		var args []string
		values := settings.Defaults()
		if override {
			args = []string{"--store", "flag-store"}
			values.Process.Store = "flag-store"
		}
		if err := flags.Parse(args); err != nil {
			t.Fatal(err)
		}
		got, err := loadServeSettings(path, flags, values)
		if err != nil {
			t.Fatal(err)
		}
		wantStore := "saved-store"
		if override {
			wantStore = "flag-store"
		}
		if got.Process.Store != wantStore || got.Logging.Level != "debug" || got.Session.Model != "from-settings" || got.Memory.MaxCacheBytes == nil || *got.Memory.MaxCacheBytes != 0 {
			t.Fatalf("bad precedence: %+v", got)
		}
	}
}

func TestConfiguredACPRuntimeRegistration(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	values := settings.Defaults()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"process":{"runtimes":["custom"],"acp":{"custom":{"command":"missing-custom-agent","args":["--acp"],"maxMessageBytes":4096}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadServeSettings(path, flags, values)
	if err != nil {
		t.Fatal(err)
	}
	rf := runtimeFlags{runtimes: got.Process.Runtimes[0], acp: got.Process.ACP}
	registry, err := rf.registry()
	if err != nil {
		t.Fatal(err)
	}
	info := registry.List()
	if len(info) != 1 || info[0].Name != "custom" {
		t.Fatalf("custom ACP runtime missing: %+v", info)
	}
}

func TestStartupSettingsValidationMatchesRuntime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"memory":{"maxCacheBytes":-1}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadServeSettings(path, flag.NewFlagSet("test", flag.ContinueOnError), settings.Defaults()); err == nil {
		t.Fatal("invalid startup settings accepted")
	}
}
