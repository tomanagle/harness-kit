//go:build windows

package process

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

const createNewProcessGroup = 0x00000200

// Configure starts cmd in a new process group so that Kill can terminate
// its whole tree without affecting the parent console.
func Configure(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNewProcessGroup
	cmd.SysProcAttr.HideWindow = true
}

// Interrupt is not supported for console-less children on Windows; callers
// fall back to Kill. (CTRL_BREAK is not reliably mapped to an interrupt by
// Node-based CLIs.)
func Interrupt(cmd *exec.Cmd) error { return ErrUnsupported }

// Kill terminates cmd and its descendants using taskkill /T, falling back
// to TerminateProcess.
func Kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errors.New("process: not started")
	}
	tk := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	tk.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := tk.Run(); err == nil {
		return nil
	}
	err := cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
