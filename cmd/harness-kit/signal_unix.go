//go:build !windows

package main

import (
	"os/signal"
	"syscall"
)

// ignoreSIGPIPE makes writes to a closed stdout fail with EPIPE instead of
// killing the process, so a vanished parent still triggers an orderly
// shutdown of runtime child processes.
func ignoreSIGPIPE() { signal.Ignore(syscall.SIGPIPE) }
