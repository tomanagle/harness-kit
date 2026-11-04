// Package process manages the sidecar's process group. This is a standalone
// copy of the core's platform helpers; the client has no dependency on core.
package process

import "errors"

var ErrUnsupported = errors.New("process: unsupported on this platform")
