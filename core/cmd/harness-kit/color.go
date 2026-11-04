package main

import (
	"io"
	"os"
	"os/exec"

	"github.com/tomanagle/harness-kit/core/process"
)

// jsonWriter returns where JSON output should go, and a function to call
// when done. When stdout is a terminal and jq is installed, output is piped
// through `jq -C` for colour. Pipes and files always get plain JSON, so
// scripts are unaffected. NO_COLOR (or HARNESS_KIT_NO_JQ) turns it off.
func jsonWriter() (io.Writer, func()) {
	plain := func() (io.Writer, func()) { return os.Stdout, func() {} }
	if os.Getenv("NO_COLOR") != "" || os.Getenv("HARNESS_KIT_NO_JQ") != "" || !stdoutIsTerminal() {
		return plain()
	}
	path, err := exec.LookPath("jq")
	if err != nil {
		return plain()
	}
	cmd := exec.Command(path, "-C", "--unbuffered", ".")
	// Its own process group: Ctrl-C cancels the turn, not the colouring, so
	// the final events still print.
	process.Configure(cmd)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return plain()
	}
	if err := cmd.Start(); err != nil {
		return plain()
	}
	return in, func() {
		in.Close()
		cmd.Wait()
	}
}

func stdoutIsTerminal() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
