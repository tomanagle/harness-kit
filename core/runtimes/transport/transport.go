// Package transport defines host-injected process dependencies for native
// runtime adapters. It contains no operating-system process implementation.
package transport

import (
	"context"
	"io"
)

// LookupOptions controls executable discovery.
type LookupOptions struct {
	// Path is an explicit executable; the default launcher verifies it exists.
	Path string
	// EnvVar may name an environment variable containing an explicit path.
	EnvVar string
	// ExtraDirs are searched after PATH, before native well-known locations.
	ExtraDirs []string
	// NoDefaultDirs disables native well-known installation directories.
	NoDefaultDirs bool
}

// Command describes one runtime invocation. Env overlays the host environment;
// a custom Launcher controls how that environment is provided to its children.
// Nil Stdin/Stdout request streaming pipes from the Child. Supplied readers and
// writers are owned by the caller and must not be closed by the launcher.
type Command struct {
	Path   string
	Args   []string
	Dir    string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Launcher locates and starts native runtime processes. Implementations must
// be safe for concurrent use. Start's context bounds startup only: successfully
// returned children outlive it and are stopped explicitly through Child.
type Launcher interface {
	LookPath(name string, options LookupOptions) (string, error)
	Start(context.Context, Command) (Child, error)
}

// Child owns one running process and its process tree. Wait is called once and
// must preserve unread stdout until the caller drains or closes its pipe.
// Stdin/Stdout return nil when Command supplied that stream. Interrupt and Kill
// must be safe concurrently with Wait; Kill must terminate descendant processes
// and unblock pipe I/O. The caller closes any returned pipes.
type Child interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Wait() error
	Interrupt() error
	Kill() error
}
