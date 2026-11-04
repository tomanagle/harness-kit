//go:build !windows && !js && !wasip1

package process

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Configure prepares cmd so that it and its descendants form a process
// group that Interrupt and Kill can signal together.
func Configure(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// Interrupt sends SIGINT to cmd's process group.
func Interrupt(cmd *exec.Cmd) error { return signalGroup(cmd, syscall.SIGINT) }

// Kill sends SIGKILL to cmd's process group.
func Kill(cmd *exec.Cmd) error { return signalGroup(cmd, syscall.SIGKILL) }

func signalGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return errors.New("process: not started")
	}
	pid := cmd.Process.Pid
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid {
		if err := syscall.Kill(-pid, sig); err == nil || errors.Is(err, syscall.ESRCH) {
			return nil
		}
	}
	err := cmd.Process.Signal(sig)
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
