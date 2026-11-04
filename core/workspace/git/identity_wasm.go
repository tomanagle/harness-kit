//go:build js || wasip1

package git

import "github.com/tomanagle/harness-kit/core/workspace"

func directoryIdentity(string) (string, error) { return "", workspace.ErrUnsupported }
