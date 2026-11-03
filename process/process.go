// Package process contains the native process host: locating CLI binaries
// and starting, interrupting and killing child processes (with their whole
// process tree) on macOS, Linux and Windows.
//
// This is the only Harness Kit package that knows about operating-system
// processes. Core packages (agent, session, event, permission, rpc) never
// import it, which is what keeps them portable to WebAssembly.
package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ErrUnsupported is returned when an operation is unavailable on this OS.
var ErrUnsupported = errors.New("process: unsupported on this platform")

// LookOptions configure LookPath.
type LookOptions struct {
	// Path, if set, is used verbatim (it must exist).
	Path string
	// EnvVar names an environment variable that may hold an explicit path,
	// e.g. "HARNESS_KIT_CODEX_PATH".
	EnvVar string
	// ExtraDirs are searched after PATH, before the default directories.
	ExtraDirs []string
	// NoDefaultDirs disables the built-in well-known install locations.
	NoDefaultDirs bool
}

// LookPath finds an executable by name. Resolution order: opts.Path, the
// opts.EnvVar environment variable, PATH, opts.ExtraDirs, then well-known
// install locations.
//
// The well-known locations matter for desktop apps: on macOS, apps started
// from Finder or the Dock do not inherit the user's shell PATH, so tools
// installed with npm, Homebrew or a native installer would otherwise not
// be found.
func LookPath(name string, opts LookOptions) (string, error) {
	if opts.Path != "" {
		return checkExecutable(opts.Path)
	}
	if opts.EnvVar != "" {
		if p := os.Getenv(opts.EnvVar); p != "" {
			path, err := checkExecutable(p)
			if err != nil {
				return "", fmt.Errorf("%s: %w", opts.EnvVar, err)
			}
			return path, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return filepath.Abs(p)
	}
	dirs := append([]string(nil), opts.ExtraDirs...)
	if !opts.NoDefaultDirs {
		dirs = append(dirs, DefaultSearchDirs()...)
	}
	for _, dir := range dirs {
		for _, cand := range candidates(dir, name) {
			if p, err := checkExecutable(cand); err == nil {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("process: %q not found in PATH or well-known locations: %w", name, exec.ErrNotFound)
}

func candidates(dir, name string) []string {
	if runtime.GOOS != "windows" {
		return []string{filepath.Join(dir, name)}
	}
	exts := []string{".exe", ".cmd", ".bat"}
	if pe := os.Getenv("PATHEXT"); pe != "" {
		exts = strings.Split(strings.ToLower(pe), ";")
	}
	out := make([]string, 0, len(exts))
	for _, e := range exts {
		if e != "" {
			out = append(out, filepath.Join(dir, name+e))
		}
	}
	return out
}

func checkExecutable(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return "", fmt.Errorf("process: %s is a directory", abs)
	}
	if runtime.GOOS != "windows" && fi.Mode()&0o111 == 0 {
		return "", fmt.Errorf("process: %s is not executable", abs)
	}
	return abs, nil
}

// DefaultSearchDirs returns well-known per-user and system install
// directories for CLI tools on this OS.
func DefaultSearchDirs() []string {
	home, _ := os.UserHomeDir()
	var dirs []string
	add := func(parts ...string) {
		if parts[0] == "" {
			return
		}
		dirs = append(dirs, filepath.Join(parts...))
	}
	switch runtime.GOOS {
	case "windows":
		add(home, ".local", "bin")
		add(os.Getenv("APPDATA"), "npm")
		add(os.Getenv("LOCALAPPDATA"), "Programs")
		add(os.Getenv("LOCALAPPDATA"), "pnpm")
		add(home, ".bun", "bin")
		add(home, "scoop", "shims")
		add(os.Getenv("ProgramFiles"), "nodejs")
	default:
		add(home, ".local", "bin")
		add(home, ".npm-global", "bin")
		add(home, ".bun", "bin")
		add(home, ".volta", "bin")
		add(home, ".local", "share", "pnpm")
		add(home, "Library", "pnpm")
		add(home, ".claude", "local")
		add(home, ".cargo", "bin")
		dirs = append(dirs, "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/home/linuxbrew/.linuxbrew/bin")
	}
	return dirs
}

var versionRE = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.\-]+)?`)

// Version runs `path args...` (default args: --version) with a timeout and
// returns the first semantic version in its output. env holds extra
// KEY=VALUE entries added to the inherited environment.
func Version(ctx context.Context, path string, env []string, args ...string) (string, error) {
	if len(args) == 0 {
		args = []string{"--version"}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	Configure(cmd)
	cmd.Cancel = func() error { return Kill(cmd) }
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("process: %s %s: %w", filepath.Base(path), strings.Join(args, " "), err)
	}
	v := versionRE.FindString(out.String())
	if v == "" {
		return "", fmt.Errorf("process: no version in output %q", strings.TrimSpace(out.String()))
	}
	return v, nil
}

// CompareVersions compares dotted numeric versions ("2.1.259"), ignoring
// pre-release suffixes. It returns -1, 0 or 1.
func CompareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
	}
	return 0
}

func versionParts(v string) []int {
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out []int
	for _, p := range strings.Split(v, ".") {
		n := 0
		for _, r := range p {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		out = append(out, n)
	}
	return out
}

// Stop ends cmd gracefully: it sends an interrupt (where supported), waits
// up to grace for waitDone to be closed, then kills the process tree.
func Stop(cmd *exec.Cmd, waitDone <-chan struct{}, grace time.Duration) {
	if cmd.Process == nil {
		return
	}
	if err := Interrupt(cmd); err == nil {
		select {
		case <-waitDone:
			return
		case <-time.After(grace):
		}
	}
	_ = Kill(cmd)
}

// Tail is an io.Writer that keeps the last max bytes written to it, for
// attaching a child's recent stderr to error messages.
type Tail struct {
	mu  sync.Mutex
	buf []byte
	max int
}

// NewTail returns a Tail retaining max bytes.
func NewTail(max int) *Tail { return &Tail{max: max} }

// Write implements io.Writer.
func (t *Tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

// String returns the retained bytes.
func (t *Tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// Suffix formats the last few retained lines as " (stderr: ...)", or ""
// if nothing was written.
func (t *Tail) Suffix() string {
	s := strings.TrimSpace(t.String())
	if s == "" {
		return ""
	}
	if lines := strings.Split(s, "\n"); len(lines) > 5 {
		s = strings.Join(lines[len(lines)-5:], "\n")
	}
	return " (stderr: " + s + ")"
}
