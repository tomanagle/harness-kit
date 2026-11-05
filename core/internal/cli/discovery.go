package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/tomanagle/harness-kit/core/agent"
)

func (a app) runtimesCmd(args []string) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("runtimes "+sub, flag.ContinueOnError)
	bf := backendFlags{runtimeFlags: runtimeFlags{version: a.version}}
	bf.register(fs)
	var out outputFlags
	out.register(fs, "text, json")
	names, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := out.resolve("text", "json"); err != nil {
		return err
	}
	asJSON := out.format == "json"
	bf.store = "memory" // runtime commands keep no history
	ctx := context.Background()
	b, err := bf.open(ctx)
	if err != nil {
		return err
	}
	defer b.Close()

	switch sub {
	case "list":
		infos, err := b.Runtimes(ctx)
		if err != nil {
			return err
		}
		if asJSON {
			return printJSON(infos)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tDISPLAY NAME\tSTREAMING\tAPPROVALS\tRESUME\tCANCEL\tCOST")
		for _, i := range infos {
			c := i.Capabilities
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", i.Name, i.DisplayName, yes(c.Streaming), yes(c.Approvals), yes(c.Resume), yes(c.Cancel), yes(c.Cost))
		}
		return tw.Flush()
	case "discover":
		ds, err := b.Discover(ctx, names)
		if err != nil {
			return err
		}
		if asJSON {
			return printJSON(ds)
		}
		printDiscovery(ds)
		return nil
	}
	return fmt.Errorf("unknown command \"runtimes %s\" (try list or discover)", sub)
}

func printDiscovery(ds []agent.Discovery) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RUNTIME\tREADY\tVERSION\tAUTH\tPATH")
	for _, d := range ds {
		auth := string(d.Auth.State)
		if d.Auth.Method != "" {
			auth += " (" + d.Auth.Method
			if d.Auth.Plan != "" {
				auth += ", " + d.Auth.Plan
			}
			auth += ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.Runtime, yes(d.Ready), dash(d.Version), auth, dash(d.Path))
	}
	tw.Flush()
	for _, d := range ds {
		for _, p := range d.Problems {
			fmt.Printf("  %s: problem: %s\n", d.Runtime, p)
		}
	}
}
