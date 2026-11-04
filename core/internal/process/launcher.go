package process

import (
	"context"
	"io"
	"os"
	"os/exec"

	"github.com/tomanagle/harness-kit/core/runtimes/transport"
)

// OSLauncher is the native implementation used when a host does not inject one.
type OSLauncher struct{}

func (OSLauncher) LookPath(name string, options transport.LookupOptions) (string, error) {
	return LookPath(name, options)
}

func (OSLauncher) Start(ctx context.Context, spec transport.Command) (transport.Child, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(spec.Path, spec.Args...)
	Configure(cmd)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Dir, spec.Stdin, spec.Stdout, spec.Stderr
	if spec.Env != nil {
		cmd.Env = append(os.Environ(), spec.Env...)
	}
	child := &osChild{cmd: cmd}
	var stdoutWriter *os.File
	cleanup := func() {
		if child.stdin != nil {
			_ = child.stdin.Close()
		}
		if child.stdout != nil {
			_ = child.stdout.Close()
		}
		if stdoutWriter != nil {
			_ = stdoutWriter.Close()
		}
	}
	var err error
	if spec.Stdin == nil {
		child.stdin, err = cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
	}
	if spec.Stdout == nil {
		// Own the read end: exec.Cmd.Wait closes StdoutPipe before adapters
		// necessarily finish reading the final protocol message.
		child.stdout, stdoutWriter, err = os.Pipe()
		if err != nil {
			cleanup()
			return nil, err
		}
		cmd.Stdout = stdoutWriter
	}
	if err := cmd.Start(); err != nil {
		cleanup()
		return nil, err
	}
	if stdoutWriter != nil {
		_ = stdoutWriter.Close()
	}
	if err := ctx.Err(); err != nil {
		_ = child.Kill()
		_ = child.Wait()
		cleanup()
		return nil, err
	}
	return child, nil
}

type osChild struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

func (p *osChild) Stdin() io.WriteCloser { return p.stdin }
func (p *osChild) Stdout() io.ReadCloser { return p.stdout }
func (p *osChild) Wait() error           { return p.cmd.Wait() }
func (p *osChild) Interrupt() error      { return Interrupt(p.cmd) }
func (p *osChild) Kill() error           { return Kill(p.cmd) }

var _ transport.Launcher = OSLauncher{}
