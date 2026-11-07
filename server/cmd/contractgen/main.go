package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/tomanagle/harness-kit/server/internal/contractgen"
)

func main() {
	check := flag.Bool("check", false, "check generated files")
	flag.Parse()
	root, err := os.Getwd()
	if err == nil {
		err = contractgen.Generate(root, *check)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
