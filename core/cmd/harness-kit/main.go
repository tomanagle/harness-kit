// Command harness-kit drives agent runtimes and serves the local sidecar protocol.
package main

import (
	"os"
	"runtime/debug"

	"github.com/tomanagle/harness-kit/core/internal/cli"
	"github.com/tomanagle/harness-kit/core/toolhost"
)

// version is set by release builds with -ldflags "-X main.version=...".
var version string

func buildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func main() {
	// Agent runtimes invoke this executable to relay MCP traffic to the tool host.
	toolhost.ServeBridgeIfRequested()
	os.Exit(cli.Run(os.Args[1:], buildVersion()))
}
