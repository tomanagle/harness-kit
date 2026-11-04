package agent

import "io/fs"

// InputFileOpener supplies image attachments to native runtime adapters. Paths
// are resolved against the session directory before Open; unlike fs.FS, names
// may be absolute operating-system paths. The adapter owns and closes each
// returned file and still enforces its size, regular-file and media-type checks.
// Implementations must be safe for concurrent use and enforce host path policy.
type InputFileOpener interface {
	Open(name string) (fs.File, error)
}
