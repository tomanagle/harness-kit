//go:build unix

package harnesskit

import (
	"os"
	"syscall"
)

// Avoid blocking on a FIFO swapped in between path inspection and open.
func openAttachment(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
