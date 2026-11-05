package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
)

// exitCode is returned by commands that want a specific process exit code.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

type app struct{ version string }

// Run dispatches one CLI invocation. The executable owns process exit and version metadata.
func Run(args []string, version string) int {
	a := app{version: version}
	args = append([]string(nil), args...)
	switch {
	case len(args) == 0 && stdinIsTerminal():
		usage(os.Stdout) // a person ran it
		return 0
	case len(args) == 0:
		args = []string{"serve"} // spawned as a sidecar with piped stdin
	case args[0] == "-h" || args[0] == "--help":
		args[0] = "help"
	case args[0] == "--version":
		args[0] = "version"
	case strings.HasPrefix(args[0], "-"):
		args = append([]string{"serve"}, args...) // `harness-kit --fake-runtime`
	}

	var err error
	switch cmd, rest := args[0], args[1:]; cmd {
	case "serve":
		err = a.serve(rest)
	case "runtimes":
		err = a.runtimesCmd(rest)
	case "discover":
		err = a.runtimesCmd(append([]string{"discover"}, rest...))
	case "prompt":
		err = a.promptCmd(rest)
	case "chat":
		err = a.chatCmd(rest)
	case "sessions", "session":
		err = a.sessionsCmd(rest)
	case "mcp":
		err = mcpCmd(rest)
	case "mcp-bridge":
		err = errors.New("mcp-bridge is started by agent runtimes to reach Harness Kit's tool host; it is not run by hand")
	case "version":
		fmt.Println("harness-kit", a.version)
	case "help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "harness-kit: unknown command %q\n\n", cmd)
		usage(os.Stderr)
		return 2
	}
	var code exitCode
	switch {
	case errors.As(err, &code):
		return int(code)
	case errors.Is(err, flag.ErrHelp):
		return 0
	case err != nil:
		fmt.Fprintln(os.Stderr, "harness-kit:", err)
		return 1
	}
	return 0
}
