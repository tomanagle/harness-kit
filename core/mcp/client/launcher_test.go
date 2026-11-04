package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/mcp"
	"github.com/tomanagle/harness-kit/core/runtimes/transport"
)

type scriptedLauncher struct {
	lookedUp  string
	lookup    transport.LookupOptions
	command   transport.Command
	child     *scriptedChild
	stall     bool
	earlyWait bool
}

func (l *scriptedLauncher) LookPath(name string, options transport.LookupOptions) (string, error) {
	l.lookedUp, l.lookup = name, options
	return "virtual/mcp-server", nil
}
func (l *scriptedLauncher) Start(ctx context.Context, command transport.Command) (transport.Child, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.command = command
	in, stdin := io.Pipe()
	stdout, out := io.Pipe()
	c := &scriptedChild{stdin: stdin, stdout: stdout, input: in, output: out, done: make(chan struct{}), killed: make(chan struct{}), earlyWait: l.earlyWait}
	l.child = c
	go func() {
		defer close(c.done)
		defer in.Close()
		defer out.Close()
		if l.stall {
			<-c.killed
			return
		}
		scanner := bufio.NewScanner(in)
		for scanner.Scan() {
			var request map[string]any
			if json.Unmarshal(scanner.Bytes(), &request) != nil || request["id"] == nil {
				continue
			}
			if result, ok := handle(request); ok {
				data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": result})
				if _, err := fmt.Fprintln(out, string(data)); err != nil {
					return
				}
			}
		}
	}()
	return c, nil
}

type scriptedChild struct {
	stdin        *io.PipeWriter
	stdout       *io.PipeReader
	input        *io.PipeReader
	output       *io.PipeWriter
	done, killed chan struct{}
	killOnce     sync.Once
	waits        atomic.Int32
	earlyWait    bool
}

func (c *scriptedChild) Stdin() io.WriteCloser { return c.stdin }
func (c *scriptedChild) Stdout() io.ReadCloser { return c.stdout }
func (c *scriptedChild) Wait() error {
	c.waits.Add(1)
	if !c.earlyWait {
		<-c.done
	}
	return nil
}
func (c *scriptedChild) Interrupt() error { return c.Kill() }
func (c *scriptedChild) Kill() error {
	c.killOnce.Do(func() { close(c.killed); _ = c.input.Close(); _ = c.output.Close() })
	return nil
}

func TestInjectedLauncherOwnsMCPStdio(t *testing.T) {
	launcher := &scriptedLauncher{}
	result, err := Probe(context.Background(), mcp.Server{
		Command: "not-an-installed-command", Args: []string{"serve"}, Cwd: "/server/cwd", Env: map[string]string{"TOKEN": "${SECRET}"},
	}, Options{Launcher: launcher, Dir: "/fallback/cwd", Lookup: func(key string) (string, bool) { return "expanded", key == "SECRET" }})
	check(t, result, err)
	if launcher.lookedUp != "not-an-installed-command" || !launcher.lookup.NoDefaultDirs {
		t.Fatalf("discovery bypassed launcher: %+v", launcher)
	}
	spec := launcher.command
	if spec.Path != "virtual/mcp-server" || spec.Dir != "/server/cwd" || !slices.Equal(spec.Args, []string{"serve"}) || !slices.Equal(spec.Env, []string{"TOKEN=expanded"}) || spec.Stdin != nil || spec.Stdout != nil || spec.Stderr == nil {
		t.Fatalf("launch contract: %+v", spec)
	}
	if launcher.child.waits.Load() != 1 {
		t.Fatalf("child Wait calls = %d", launcher.child.waits.Load())
	}
}

func TestInjectedLauncherWaitPreservesPendingResponses(t *testing.T) {
	// A child can finish before its final bytes are parsed. Wait completion
	// must not beat the JSON-RPC reader to a buffered successful response.
	launcher := &scriptedLauncher{earlyWait: true}
	result, err := Probe(context.Background(), mcp.Server{Command: "virtual"}, Options{Launcher: launcher})
	check(t, result, err)
}

func TestInjectedLauncherCancellationUnblocksStdioWrites(t *testing.T) {
	launcher := &scriptedLauncher{stall: true}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := Connect(ctx, mcp.Server{Command: "virtual"}, Options{Launcher: launcher})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startup cancellation: %v", err)
	}
	select {
	case <-launcher.child.killed:
	default:
		t.Fatal("blocked child was not terminated")
	}
	if launcher.child.waits.Load() != 1 {
		t.Fatalf("child Wait calls = %d", launcher.child.waits.Load())
	}
}
