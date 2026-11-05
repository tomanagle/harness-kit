//go:build !unix

package harnesskit

import "os"

func openAttachment(path string) (*os.File, error) { return os.Open(path) }
