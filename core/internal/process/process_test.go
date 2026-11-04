package process

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.1.259", "2.1.259", 0},
		{"2.1.316", "2.1.259", 1},
		{"0.9.0", "0.166.0", -1},
		{"1.2", "1.2.0", 0},
		{"1.2.3-beta.1", "1.2.3", 0},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestLookPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX executable bit")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "hk-test-tool")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho hk-test-tool 1.2.3\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if p, err := LookPath("hk-test-tool", LookOptions{ExtraDirs: []string{dir}, NoDefaultDirs: true}); err != nil || p != bin {
		t.Fatalf("extra dirs: %q %v", p, err)
	}
	t.Setenv("HK_TEST_TOOL_PATH", bin)
	if p, err := LookPath("nonexistent-name", LookOptions{EnvVar: "HK_TEST_TOOL_PATH"}); err != nil || p != bin {
		t.Fatalf("env var: %q %v", p, err)
	}
	if _, err := LookPath("hk-definitely-missing", LookOptions{NoDefaultDirs: true}); err == nil {
		t.Fatal("missing binary should fail")
	}
	if _, err := LookPath("x", LookOptions{Path: dir}); err == nil {
		t.Fatal("directory should not be accepted")
	}

	v, err := Version(context.Background(), bin, nil)
	if err != nil || v != "1.2.3" {
		t.Fatalf("version: %q %v", v, err)
	}
}

func TestStopKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell")
	}
	// sh traps INT and ignores it, forcing the kill path; its child sleep
	// must die with it.
	cmd := exec.Command("sh", "-c", "trap '' INT; sleep 30 & wait")
	Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	start := time.Now()
	Stop(cmd, done, 200*time.Millisecond)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process group not killed")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("stop took too long")
	}
}
