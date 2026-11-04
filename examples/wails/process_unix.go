//go:build !windows && !js && !wasip1

package main

import (
	"errors"
	"os/exec"
	"syscall"
)

// The raw relay owns its sidecar and descendants; process supervision belongs
// to the embedding application rather than the portable SDK's public API.
func configureSidecarProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killSidecarProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return cmd.Process.Kill()
	}
	return nil
}
