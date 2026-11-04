//go:build js || wasip1

package process

import "os/exec"

// Configure is a no-op: WebAssembly hosts cannot spawn processes.
func Configure(cmd *exec.Cmd) {}

// Interrupt is unsupported on WebAssembly.
func Interrupt(cmd *exec.Cmd) error { return ErrUnsupported }

// Kill is unsupported on WebAssembly.
func Kill(cmd *exec.Cmd) error { return ErrUnsupported }
