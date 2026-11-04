package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/tomanagle/harness-kit/core/process"
)

// rpcEvent is the Wails event carrying sidecar -> frontend protocol lines.
const rpcEvent = "harness-kit:rpc"

// allowed lists the protocol methods the frontend may call. The sidecar
// never offers raw shell or filesystem access, but an allowlist keeps the
// webview's reach explicit (e.g. drop "session.create" to pin sessions to
// backend-chosen configurations).
var allowed = map[string]bool{
	"initialize": true, "shutdown": true,
	"runtime.list": true, "runtime.discover": true, "runtime.sessions": true,
	"session.create": true, "session.get": true, "session.list": true, "session.restore": true, "session.rename": true, "session.configure": true,
	"session.prompt": true, "session.cancel": true, "session.close": true, "session.events": true,
	"approval.respond": true, "approval.list": true,
	"tools.register": true, "tools.unregister": true, "tools.list": true,
	"mcp.list":        true,
	"$/cancelRequest": true,
}

// Bridge runs the harness-kit sidecar and forwards protocol lines between
// it and the frontend unchanged, so the frontend can use @tomanagle/harness-kit
// exactly as an Electron app does.
type Bridge struct {
	bin   string
	ctx   context.Context
	cmd   *exec.Cmd
	mu    sync.Mutex
	stdin io.WriteCloser
	done  chan struct{}
}

// NewBridge returns a bridge for the sidecar binary at bin.
func NewBridge(bin string) *Bridge { return &Bridge{bin: bin, done: make(chan struct{})} }

func (b *Bridge) startup(ctx context.Context) {
	b.ctx = ctx
	b.cmd = exec.Command(b.bin, "serve", "--fake-runtime", "--kit-mcp", "--store", storeDir())
	process.Configure(b.cmd)
	b.cmd.Stderr = os.Stderr
	stdin, _ := b.cmd.StdinPipe()
	stdout, _ := b.cmd.StdoutPipe()
	b.stdin = stdin
	if err := b.cmd.Start(); err != nil {
		wailsruntime.LogErrorf(ctx, "start sidecar: %v", err)
		close(b.done)
		return
	}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 32<<20)
		for sc.Scan() {
			wailsruntime.EventsEmit(ctx, rpcEvent, sc.Text())
		}
		_ = b.cmd.Wait()
		close(b.done)
	}()
}

// Send forwards one protocol line from the frontend to the sidecar.
func (b *Bridge) Send(line string) error {
	forward, reply, err := filter(line)
	if err != nil {
		return err
	}
	if reply != "" {
		wailsruntime.EventsEmit(b.ctx, rpcEvent, reply) // answer locally so the client doesn't hang
	}
	if !forward {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_, err = io.WriteString(b.stdin, line+"\n")
	return err
}

// filter decides what to do with a line from the frontend: forward allowed
// requests and notifications, and responses to the sidecar's own requests
// (tool.call); answer disallowed requests with an error; drop disallowed
// notifications.
func filter(line string) (forward bool, reply string, err error) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return false, "", err
	}
	if msg.Method == "" || allowed[msg.Method] {
		return true, "", nil
	}
	if msg.ID != nil {
		return false, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not allowed by host: %s"}}`, msg.ID, msg.Method), nil
	}
	return false, "", nil
}

func (b *Bridge) shutdown(context.Context) {
	if b.stdin != nil {
		b.stdin.Close() // the sidecar closes all sessions and exits
	}
	select {
	case <-b.done:
	case <-time.After(10 * time.Second):
		_ = process.Kill(b.cmd)
	}
}

// storeDir keeps the example's session history between runs.
func storeDir() string {
	if d := os.Getenv("HARNESS_KIT_STORE"); d != "" {
		return d
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "harness-kit-wails-example", "sessions")
}

func sidecarPath() string {
	if p := os.Getenv("HARNESS_KIT_BIN"); p != "" {
		return p
	}
	exe := "harness-kit"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	// Next to the app binary when packaged, else the repo's core/bin/.
	if self, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(self), exe); fileExists(p) {
			return p
		}
	}
	return filepath.Join("..", "..", "core", "bin", exe)
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
