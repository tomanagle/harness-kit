package cli

import (
	"errors"

	"github.com/tomanagle/harness-kit/core/internal/rpcclient"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
)

func isUnsupported(err error) bool { return isReason(err, protocol.ReasonUnsupported) }

// isReason reports whether err is a sidecar error with the given reason.
func isReason(err error, reason string) bool {
	var ce *client.Error
	return errors.As(err, &ce) && ce.Reason == reason
}
