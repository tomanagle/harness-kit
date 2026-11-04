package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tomanagle/harness-kit/core/mcp"
	mcpclient "github.com/tomanagle/harness-kit/core/mcp/client"
	"github.com/tomanagle/harness-kit/core/runtimes/codex"
	"github.com/tomanagle/harness-kit/core/session"
)

// kitResolver is the resolver for the user and project config files.
func kitResolver() (mcp.Resolver, error) {
	user, err := mcp.UserConfigPath()
	if err != nil {
		return mcp.Resolver{}, err
	}
	trust, err := mcp.TrustPath()
	if err != nil {
		return mcp.Resolver{}, err
	}
	return mcp.Resolver{UserFile: user, Project: true, Trust: mcp.TrustStore{Path: trust}}, nil
}

// kitPrepare returns a session.Options.Prepare hook that merges the user
// and trusted project MCP config into each new session.
func kitPrepare(r mcp.Resolver) func(context.Context, *session.CreateParams) ([]string, error) {
	return func(_ context.Context, p *session.CreateParams) ([]string, error) {
		var own mcp.Config
		if p.Config.MCP != nil {
			own = *p.Config.MCP
		}
		cwd := p.Config.Cwd
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
		res, err := r.Resolve(cwd, p.Runtime, own)
		if err != nil {
			return nil, err
		}
		if res.Config.IsZero() {
			p.Config.MCP = nil
		} else {
			c := res.Config
			p.Config.MCP = &c
		}
		return res.Warnings, nil
	}
}

// sessionMCP builds the session-level MCP layer from --mcp and
// --no-native-mcp.
func sessionMCP(file string, noNative bool) (*mcp.Config, error) {
	var c mcp.Config
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		if c, err = mcp.Parse(b); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
	}
	if noNative {
		f := false
		c.InheritNative = &f
	}
	if c.IsZero() {
		return nil, nil
	}
	return &c, nil
}

func mcpCmd(args []string) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "list", "ls":
		return mcpList(args)
	case "add":
		return mcpAdd(args)
	case "remove", "rm":
		return mcpRemove(args)
	case "trust", "untrust":
		return mcpTrust(args, sub == "trust")
	case "test":
		return mcpTest(args)
	}
	return fmt.Errorf("unknown command \"mcp %s\" (try list, add, remove, trust, untrust or test)", sub)
}

func projectFlag(fs *flag.FlagSet) (*bool, *string) {
	return fs.Bool("project", false, "use the project's harness-kit.json instead of your user config"),
		fs.String("cwd", ".", "directory used to find the project's harness-kit.json")
}

// configFile returns the file to edit for --project / user scope. For the
// project scope a missing file is created in cwd.
func configFile(project bool, cwd string) (string, error) {
	if !project {
		return mcp.UserConfigPath()
	}
	if p, ok := mcp.FindProjectFile(cwd); ok {
		return p, nil
	}
	abs, err := filepath.Abs(cwd)
	return filepath.Join(abs, mcp.ProjectFileName), err
}

func mcpList(args []string) error {
	fs := flag.NewFlagSet("mcp list", flag.ContinueOnError)
	cwd := fs.String("cwd", ".", "directory used to find the project's harness-kit.json")
	runtimeName := fs.String("runtime", "", "with --native: whose own servers to list (codex)")
	native := fs.Bool("native", false, "list the servers configured in the runtime itself")
	var out outputFlags
	out.register(fs, "text, json")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	if err := out.resolve("text", "json"); err != nil {
		return err
	}
	dir, _ := filepath.Abs(*cwd)
	if *native {
		if *runtimeName != "codex" {
			return fmt.Errorf("--native supports -runtime codex; for Claude Code run `claude mcp list`")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		names, err := codex.New().NativeMCPServers(ctx, dir)
		if err != nil {
			return err
		}
		if out.format == "json" {
			return printJSON(names)
		}
		for _, n := range names {
			fmt.Println(n)
		}
		fmt.Fprintf(os.Stderr, "\n%d server(s) configured in Codex itself; they load in every session unless inheritNative is false (--no-native-mcp)\n", len(names))
		return nil
	}

	r, err := kitResolver()
	if err != nil {
		return err
	}
	ov, err := r.Overview(dir)
	if err != nil {
		return err
	}
	rows := ov.Servers
	if out.format == "json" {
		return printJSON(ov)
	}
	if len(rows) == 0 {
		fmt.Fprintf(os.Stderr, "no Harness Kit MCP servers. Add one with:\n  harness-kit mcp add NAME -- COMMAND [ARGS...]\n  harness-kit mcp add NAME --url https://...\n")
	} else {
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSOURCE\tTYPE\tTARGET\tAPPROVAL\tTOOLS\tRUNTIMES\tSTATUS")
		for _, r := range rows {
			s := r.Server
			typ, target := "stdio", strings.TrimSpace(s.Command+" "+strings.Join(s.Args, " "))
			if s.IsHTTP() {
				typ, target = "http", s.URL
			}
			tools := "all"
			if len(s.Tools.Allow) > 0 {
				tools = strings.Join(s.Tools.Allow, ",")
			}
			if len(s.Tools.Deny) > 0 {
				tools += " -" + strings.Join(s.Tools.Deny, ",-")
			}
			status := "ok"
			if !r.Loaded {
				status = r.Note
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.Source, typ, clip(target, 48), s.EffectiveApproval(), tools, dash(strings.Join(s.Runtimes, ",")), status)
		}
		tw.Flush()
	}
	fmt.Fprintf(os.Stderr, "\nuser config:    %s\n", r.UserFile)
	if ov.ProjectFile != "" {
		state := "trusted"
		if !ov.ProjectTrusted {
			state = "NOT trusted"
		}
		fmt.Fprintf(os.Stderr, "project config: %s (%s)\n", ov.ProjectFile, state)
	}
	if ov.InheritNative {
		fmt.Fprintln(os.Stderr, "native servers: also loaded (see `harness-kit mcp list --native -runtime codex`; disable with --no-native-mcp)")
	} else {
		fmt.Fprintln(os.Stderr, "native servers: not loaded (inheritNative is false)")
	}
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func mcpAdd(args []string) error {
	fs := flag.NewFlagSet("mcp add", flag.ContinueOnError)
	project, cwd := projectFlag(fs)
	urlFlag := fs.String("url", "", "streamable HTTP endpoint (instead of a command)")
	var envs, headers, runtimes multiFlag
	fs.Var(&envs, "env", "environment for a stdio server: KEY=VALUE, or just KEY to pass ${KEY} through (repeatable)")
	fs.Var(&headers, "header", "HTTP header: Name=Value (repeatable; use ${VAR} for secrets)")
	bearer := fs.String("bearer-env", "", "send Authorization: Bearer ${VAR} for an http server")
	allow := fs.String("allow", "", "comma-separated tools that may be used (default: all)")
	deny := fs.String("deny", "", "comma-separated tools that may never be used")
	approval := fs.String("approval", "", "auto (run tools without asking, default) or ask")
	fs.Var(&runtimes, "runtime", "only load for this runtime (repeatable)")
	startup := fs.Float64("startup-timeout", 0, "seconds to wait for the server to start")
	toolTimeout := fs.Float64("tool-timeout", 0, "seconds a tool call may take")
	cwdServer := fs.String("server-cwd", "", "working directory for a stdio server")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("usage: harness-kit mcp add NAME [flags] -- COMMAND [ARGS...]   or   harness-kit mcp add NAME --url URL [flags]")
	}
	name, command := pos[0], pos[1:]
	s := mcp.Server{
		Tools: mcp.ToolFilter{Allow: splitList(*allow), Deny: splitList(*deny)}, Approval: mcp.Approval(*approval),
		Runtimes: runtimes, StartupTimeoutSec: *startup, ToolTimeoutSec: *toolTimeout,
	}
	switch {
	case *urlFlag != "" && len(command) > 0:
		return errors.New("give either a command or --url, not both")
	case *urlFlag != "":
		s.Type, s.URL = "http", *urlFlag
		for _, h := range headers {
			k, v, ok := strings.Cut(h, "=")
			if !ok {
				return fmt.Errorf("--header %q: want Name=Value", h)
			}
			if s.Headers == nil {
				s.Headers = map[string]string{}
			}
			s.Headers[k] = v
		}
		if *bearer != "" {
			if s.Headers == nil {
				s.Headers = map[string]string{}
			}
			s.Headers["Authorization"] = "Bearer ${" + *bearer + "}"
		}
	case len(command) > 0:
		s.Command, s.Args, s.Cwd = command[0], command[1:], *cwdServer
		for _, e := range envs {
			k, v, ok := strings.Cut(e, "=")
			if !ok {
				v = "${" + k + "}" // pass the variable through by reference
			}
			if s.Env == nil {
				s.Env = map[string]string{}
			}
			s.Env[k] = v
		}
	default:
		return errors.New("give the server's command after --, or --url for a remote server")
	}
	if (*urlFlag != "" && len(envs) > 0) || (len(command) > 0 && (len(headers) > 0 || *bearer != "")) {
		return errors.New("--env applies to command servers; --header/--bearer-env to --url servers")
	}
	file, err := configFile(*project, *cwd)
	if err != nil {
		return err
	}
	c, _, err := mcp.LoadFile(file)
	if err != nil {
		return err
	}
	if c.Servers == nil {
		c.Servers = map[string]mcp.Server{}
	}
	_, replaced := c.Servers[name]
	c.Servers[name] = s
	if err := c.Validate(); err != nil {
		return err
	}
	content, err := mcp.SaveFile(file, c)
	if err != nil {
		return err
	}
	if *project {
		// You just wrote it yourself, so its new content is trusted.
		if err := trustStore().Trust(file, content); err != nil {
			return err
		}
	}
	verb := "added"
	if replaced {
		verb = "updated"
	}
	fmt.Fprintf(os.Stderr, "%s MCP server %q in %s\ncheck it with: harness-kit mcp test %s\n", verb, name, file, name)
	return nil
}

func trustStore() mcp.TrustStore {
	p, _ := mcp.TrustPath()
	return mcp.TrustStore{Path: p}
}

func mcpRemove(args []string) error {
	fs := flag.NewFlagSet("mcp remove", flag.ContinueOnError)
	project, cwd := projectFlag(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: harness-kit mcp remove NAME [--project]")
	}
	file, err := configFile(*project, *cwd)
	if err != nil {
		return err
	}
	c, _, err := mcp.LoadFile(file)
	if err != nil {
		return err
	}
	if _, ok := c.Servers[pos[0]]; !ok {
		return fmt.Errorf("no MCP server %q in %s", pos[0], file)
	}
	delete(c.Servers, pos[0])
	content, err := mcp.SaveFile(file, c)
	if err != nil {
		return err
	}
	if *project {
		if err := trustStore().Trust(file, content); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "removed MCP server %q from %s\n", pos[0], file)
	return nil
}

func mcpTrust(args []string, trust bool) error {
	fs := flag.NewFlagSet("mcp trust", flag.ContinueOnError)
	cwd := fs.String("cwd", ".", "directory used to find the project's harness-kit.json")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	file, ok := mcp.FindProjectFile(*cwd)
	if !ok {
		return fmt.Errorf("no %s in %s or its parents", mcp.ProjectFileName, *cwd)
	}
	if !trust {
		if err := trustStore().Untrust(file); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "untrusted %s\n", file)
		return nil
	}
	c, content, err := mcp.LoadFile(file)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s defines %d MCP server(s):\n", file, len(c.Servers))
	for _, name := range c.Names() {
		s := c.Servers[name]
		if s.IsHTTP() {
			fmt.Fprintf(os.Stderr, "  %s: %s\n", name, s.URL)
		} else {
			fmt.Fprintf(os.Stderr, "  %s: %s %s\n", name, s.Command, strings.Join(s.Args, " "))
		}
	}
	if err := trustStore().Trust(file, content); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "trusted. Any change to the file needs trusting again.")
	return nil
}

func mcpTest(args []string) error {
	fs := flag.NewFlagSet("mcp test", flag.ContinueOnError)
	cwd := fs.String("cwd", ".", "directory used to find the project's harness-kit.json")
	file := fs.String("mcp", "", "also read servers from this file")
	var out outputFlags
	out.register(fs, "text, json")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := out.resolve("text", "json"); err != nil {
		return err
	}
	r, err := kitResolver()
	if err != nil {
		return err
	}
	extra, err := sessionMCP(*file, false)
	if err != nil {
		return err
	}
	var own mcp.Config
	if extra != nil {
		own = *extra
	}
	dir, _ := filepath.Abs(*cwd)
	res, err := r.Resolve(dir, "", own)
	if err != nil {
		return err
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	names := pos
	if len(names) == 0 {
		names = res.All.Names()
	}
	if len(names) == 0 {
		return errors.New("no MCP servers configured (see harness-kit mcp add)")
	}
	failed := 0
	results := map[string]any{}
	for _, name := range names {
		s, ok := res.All.Servers[name]
		if !ok {
			return fmt.Errorf("no MCP server %q (untrusted project servers can't be tested; see harness-kit mcp list)", name)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		probe, err := mcpclient.Probe(ctx, s, mcpclient.Options{Dir: dir})
		cancel()
		if out.format == "json" {
			if err != nil {
				results[name] = map[string]any{"ok": false, "error": err.Error()}
			} else {
				results[name] = map[string]any{"ok": true, "result": probe}
			}
		} else if err != nil {
			fmt.Printf("✗ %s: %v\n", name, err)
		} else {
			fmt.Printf("✓ %s: %s %s, %d tool(s)\n", name, dash(probe.ServerName), probe.ServerVersion, len(probe.Tools))
			for _, t := range probe.Tools {
				mark := ""
				if len(s.Tools.Allow) > 0 && !contains(s.Tools.Allow, t.Name) || contains(s.Tools.Deny, t.Name) {
					mark = "  (filtered out)"
				}
				fmt.Printf("    %s%s  %s\n", t.Name, mark, clip(strings.Join(strings.Fields(t.Description), " "), 70))
			}
		}
		if err != nil {
			failed++
		}
	}
	if out.format == "json" {
		if err := printJSON(results); err != nil {
			return err
		}
	}
	if failed > 0 {
		return exitCode(1)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
