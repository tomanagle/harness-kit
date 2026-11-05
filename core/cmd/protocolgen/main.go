// Command protocolgen generates the schema and clients from the core wire contract.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/tomanagle/harness-kit/core/internal/protocolgen"
)

func main() {
	check := flag.Bool("check", false, "fail if committed schema or generated clients are stale")
	flag.Parse()
	if err := protocolgen.Run(*check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
