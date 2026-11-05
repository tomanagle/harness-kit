package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// parseArgs parses flags that may be interspersed with positional
// arguments ("prompt hello -runtime fake"). Everything after "--" is
// positional.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var tail []string
	for i, a := range args {
		if a == "--" {
			args, tail = args[:i], args[i+1:]
			break
		}
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return append(pos, tail...), nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// outputFlags implements --output (alias -o) and the --json shorthand.
type outputFlags struct {
	format string
	json   bool
}

func (o *outputFlags) register(fs *flag.FlagSet, formats string) {
	fs.StringVar(&o.format, "output", "text", "output format: "+formats)
	fs.StringVar(&o.format, "o", "text", "shorthand for --output")
	fs.BoolVar(&o.json, "json", false, "shorthand for --output json")
}

func (o *outputFlags) resolve(allowed ...string) error {
	if o.json {
		if o.format != "text" && o.format != "json" {
			return fmt.Errorf("--json conflicts with --output %s", o.format)
		}
		o.format = "json"
	}
	for _, a := range allowed {
		if o.format == a {
			return nil
		}
	}
	return fmt.Errorf("--output must be one of: %s", strings.Join(allowed, ", "))
}
