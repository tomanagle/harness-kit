// Package processtest provides in-memory children for protocol adapter tests.
package processtest

import (
	"context"
	"io"
	"sync"

	"github.com/tomanagle/harness-kit/core/runtimes/transport"
)

// Launcher runs a script for every command without looking up or spawning an OS process.
type Launcher struct {
	Run      func(context.Context, transport.Command, io.Reader, io.Writer) error
	mu       sync.Mutex
	commands []transport.Command
}

func (l *Launcher) LookPath(name string, options transport.LookupOptions) (string, error) {
	if options.Path != "" {
		return options.Path, nil
	}
	return "virtual/" + name, nil
}

func (l *Launcher) Commands() []transport.Command {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]transport.Command(nil), l.commands...)
}

func (l *Launcher) Start(ctx context.Context, spec transport.Command) (transport.Child, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	spec.Args = append([]string(nil), spec.Args...)
	spec.Env = append([]string(nil), spec.Env...)
	l.mu.Lock()
	l.commands = append(l.commands, spec)
	l.mu.Unlock()
	life, cancel := context.WithCancel(context.Background())
	c := &child{cancel: cancel, done: make(chan struct{})}
	input, output := spec.Stdin, spec.Stdout
	if input == nil {
		c.input, c.stdin = io.Pipe()
		input = c.input
	}
	if output == nil {
		c.stdout, c.output = io.Pipe()
		output = c.output
	}
	go func() {
		c.err = l.Run(life, spec, input, output)
		if c.input != nil {
			_ = c.input.Close()
		}
		if c.output != nil {
			_ = c.output.Close()
		}
		close(c.done)
	}()
	return c, nil
}

type child struct {
	stdin  *io.PipeWriter
	stdout *io.PipeReader
	input  *io.PipeReader
	output *io.PipeWriter
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func (c *child) Stdin() io.WriteCloser {
	if c.stdin == nil {
		return nil
	}
	return c.stdin
}
func (c *child) Stdout() io.ReadCloser {
	if c.stdout == nil {
		return nil
	}
	return c.stdout
}
func (c *child) Wait() error      { <-c.done; return c.err }
func (c *child) Interrupt() error { return c.Kill() }
func (c *child) Kill() error {
	c.cancel()
	if c.input != nil {
		_ = c.input.Close()
	}
	if c.output != nil {
		_ = c.output.Close()
	}
	return nil
}
