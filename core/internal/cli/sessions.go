package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
	"github.com/tomanagle/harness-kit/core/workspace"
)

func isSessionID(s string) bool { return strings.HasPrefix(s, "ses_") }

func (a app) sessionsCmd(args []string) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("sessions "+sub, flag.ContinueOnError)
	bf := backendFlags{runtimeFlags: runtimeFlags{version: a.version}}
	bf.register(fs)
	var out outputFlags
	out.register(fs, "text, json")
	runtimeName := fs.String("runtime", "", "only this runtime (required with --native)")
	cwd := fs.String("cwd", "", "only sessions in this working directory")
	limit := fs.Int("limit", 20, "maximum number of sessions (0 = all)")
	native := fs.Bool("native", false, "list the runtime's own stored sessions, including ones started outside Harness Kit")
	query := fs.String("query", "", "case-insensitive title substring")
	transcript := fs.Bool("search-transcript", false, "also search saved event contents")
	tags := fs.String("tags", "", "comma-separated tags that must all match")
	archived := fs.String("archived", "all", "archive filter: all, true, false")
	forkTitle := fs.String("title", "", "title for the fork (defaults to parent title)")
	forkSettingsRaw := fs.String("session-settings", "", "shared settings JSON for the fork, including workspace and budget overrides")
	diffBytes := fs.Int64("max-bytes", 0, "maximum bytes returned by sessions diff (0 keeps the recorded limit)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := out.resolve("text", "json"); err != nil {
		return err
	}
	var forkSettings settings.Patch
	if *forkSettingsRaw != "" {
		if sub != "fork" {
			return errors.New("--session-settings is only valid with sessions fork")
		}
		if err := json.Unmarshal([]byte(*forkSettingsRaw), &forkSettings); err != nil {
			return fmt.Errorf("--session-settings: %w", err)
		}
	}
	if *cwd != "" {
		if *cwd, err = filepath.Abs(*cwd); err != nil {
			return err
		}
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "runtimes" })
	if !explicit && *runtimeName != "" {
		bf.runtimes = *runtimeName
	}
	if *native {
		bf.store = "memory"
	}
	ctx := context.Background()
	b, err := bf.open(ctx)
	if err != nil {
		return err
	}
	defer b.Close()

	needID := func() (string, error) {
		if len(pos) != 1 {
			return "", fmt.Errorf("usage: harness-kit sessions %s <session-id>", sub)
		}
		return pos[0], nil
	}
	switch sub {
	case "diff":
		if len(pos) != 2 {
			return errors.New("usage: harness-kit sessions diff <session-id> <turn-id> [--max-bytes N]")
		}
		diff, err := b.Diff(ctx, pos[0], pos[1], workspace.DiffOptions{MaxBytes: *diffBytes})
		if err != nil {
			return err
		}
		if out.format == "json" {
			return printJSON(diff)
		}
		fmt.Fprint(os.Stdout, diff.WorkingTree)
		if diff.Index != "" {
			fmt.Fprintln(os.Stdout, "\nStaged changes:")
			fmt.Fprint(os.Stdout, diff.Index)
		}
		if !diff.Complete {
			return fmt.Errorf("incomplete turn diff: %s", diff.Error)
		}
		if diff.Truncated {
			fmt.Fprintln(os.Stderr, "diff truncated by the configured size limit")
		}
		if diff.EncodingLossy {
			fmt.Fprintln(os.Stderr, "some file content could not be displayed as text")
		}
		if diff.WorkingTree == "" && diff.Index == "" {
			fmt.Fprintln(os.Stdout, "No file changes during this turn.")
		}
		return nil
	case "remove-workspace":
		id, err := needID()
		if err != nil {
			return err
		}
		if err := b.RemoveWorkspace(ctx, id); err != nil {
			return err
		}
		if out.format == "json" {
			return printJSON(map[string]bool{"ok": true})
		}
		fmt.Fprintln(os.Stdout, "Removed the session's clean, owned worktree.")
		return nil
	case "list":
		if *native {
			return listNativeSessions(ctx, b, *runtimeName, *cwd, *limit, out.format == "json")
		}
		if !bf.persistent() {
			return errors.New("no session store (--store memory); use --native to ask a runtime for its sessions")
		}
		var archiveFilter *bool
		switch *archived {
		case "all":
		case "true", "false":
			value := *archived == "true"
			archiveFilter = &value
		default:
			return errors.New("--archived must be all, true or false")
		}
		var tagFilter []string
		if *tags != "" {
			tagFilter = strings.Split(*tags, ",")
		}
		return listSessions(ctx, b, session.ListOptions{Runtime: *runtimeName, Cwd: *cwd, Limit: *limit, Query: *query, SearchTranscript: *transcript, Tags: tagFilter, Archived: archiveFilter}, out.format == "json")
	case "show":
		id, err := needID()
		if err != nil {
			return err
		}
		info, err := b.Get(ctx, id)
		if err != nil {
			return err
		}
		if out.format == "json" {
			return printJSON(info)
		}
		effort := ""
		if info.Config != nil {
			effort = info.Config.Effort
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		for _, kv := range [][2]string{
			{"session", info.ID}, {"runtime", info.Runtime}, {"runtime session", dash(info.RuntimeSessionID)},
			{"archived", fmt.Sprint(info.Archived)}, {"tags", strings.Join(session.Tags(info), ", ")}, {"forked from", dash(info.ForkedFrom)}, {"fork pending", fmt.Sprint(info.ForkPending)},
			{"title", dash(info.Title)}, {"cwd", dash(info.Cwd)}, {"model", dash(info.Model)}, {"effort", dash(effort)},
			{"turns", fmt.Sprint(info.Turns)}, {"last status", dash(string(info.LastStatus))}, {"state", string(info.State)},
			{"created", info.CreatedAt.Local().Format(time.DateTime)}, {"updated", info.UpdatedAt.Local().Format(time.DateTime)},
		} {
			fmt.Fprintf(tw, "%s:\t%s\n", kv[0], kv[1])
		}
		tw.Flush()
		fmt.Printf("\nresume with: harness-kit prompt -resume %s \"...\"\n", info.ID)
		return nil
	case "archive", "unarchive", "fork", "tags":
		if len(pos) == 0 || (sub != "tags" && len(pos) != 1) {
			return fmt.Errorf("usage: harness-kit sessions %s <session-id> [tags...]", sub)
		}
		var info session.Info
		switch sub {
		case "archive", "unarchive":
			info, err = b.Archive(ctx, pos[0], sub == "archive")
		case "fork":
			info, err = b.Fork(ctx, pos[0], session.ForkParams{Title: *forkTitle, Settings: forkSettings})
		case "tags":
			info, err = b.SetTags(ctx, pos[0], pos[1:])
		}
		if err != nil {
			return err
		}
		if out.format == "json" {
			return printJSON(info)
		}
		fmt.Fprintln(os.Stdout, info.ID)
		return nil
	case "rename", "mv":
		if len(pos) < 2 {
			return errors.New(`usage: harness-kit sessions rename <session-id> "new title"`)
		}
		info, err := b.Rename(ctx, pos[0], strings.Join(pos[1:], " "))
		if err != nil {
			return err
		}
		if out.format == "json" {
			return printJSON(info)
		}
		fmt.Fprintf(os.Stderr, "renamed %s to %q\n", info.ID, info.Title)
		return nil
	case "settings":
		if len(pos) < 1 || len(pos) > 2 {
			return errors.New(`usage: harness-kit sessions settings <session-id> ['{"budget":{"maxSessionTokens":10000}}']`)
		}
		target := settings.Target{SessionID: pos[0]}
		var snapshot settings.Snapshot
		if len(pos) == 1 {
			snapshot, err = b.Settings(ctx, target)
		} else {
			var patch settings.Patch
			if err = json.Unmarshal([]byte(pos[1]), &patch); err != nil {
				return err
			}
			snapshot, err = b.ConfigureSettings(ctx, settings.ConfigureParams{Target: target, Patch: patch})
		}
		if err != nil {
			return err
		}
		return printJSON(snapshot)
	case "configure":
		if len(pos) != 2 {
			return errors.New(`usage: harness-kit sessions configure <session-id> '{"model":"model-name","effort":"high"}'`)
		}
		patch, err := parseConfigPatch(pos[1])
		if err != nil {
			return err
		}
		info, err := b.Configure(ctx, pos[0], patch)
		if err != nil {
			return err
		}
		if out.format == "json" {
			return printJSON(info)
		}
		fmt.Fprintf(os.Stderr, "updated defaults for %s\n", info.ID)
		return nil
	case "delete", "rm":
		id, err := needID()
		if err != nil {
			return err
		}
		if err := b.Delete(ctx, id); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "deleted %s (the runtime's own transcript is untouched)\n", id)
		return nil
	}
	return fmt.Errorf("unknown command \"sessions %s\" (try list, show, rename, configure, fork, archive, unarchive, tags or delete)", sub)
}

func listSessions(ctx context.Context, b backend, options session.ListOptions, asJSON bool) error {
	list, err := b.Query(ctx, options)
	if err != nil {
		return err
	}
	if asJSON {
		if list == nil {
			list = []session.Info{}
		}
		return printJSON(list)
	}
	if len(list) == 0 {
		fmt.Fprintln(os.Stderr, "no sessions yet; start one with: harness-kit prompt -runtime codex \"...\"")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tRUNTIME\tUPDATED\tTURNS\tLAST\tCWD\tTITLE")
	for _, s := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", s.ID, s.Runtime, ago(s.UpdatedAt), s.Turns, dash(string(s.LastStatus)), shortPath(s.Cwd), clip(s.Title, 50))
	}
	tw.Flush()
	fmt.Fprintln(os.Stderr, "\ncontinue one with: harness-kit prompt -resume <SESSION> \"...\"")
	return nil
}

func listNativeSessions(ctx context.Context, b backend, runtimeName, cwd string, limit int, asJSON bool) error {
	if runtimeName == "" {
		return errors.New("--native needs -runtime (e.g. -runtime codex)")
	}
	list, err := b.RuntimeSessions(ctx, runtimeName, agent.ListSessionsOptions{Cwd: cwd, Limit: limit})
	if errors.Is(err, agent.ErrUnsupported) || isUnsupported(err) {
		return fmt.Errorf("%s does not offer a way to list its sessions outside its Python/TypeScript SDKs; "+
			"`harness-kit sessions` shows the %s sessions you ran through Harness Kit", runtimeName, runtimeName)
	}
	if err != nil {
		return err
	}
	if asJSON {
		if list == nil {
			list = []agent.RuntimeSession{}
		}
		return printJSON(list)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tUPDATED\tSOURCE\tCWD\tTITLE")
	for _, s := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.ID, ago(s.UpdatedAt), dash(s.Source), shortPath(s.Cwd), clip(s.Title, 50))
	}
	tw.Flush()
	fmt.Fprintf(os.Stderr, "\ncontinue one with: harness-kit prompt -runtime %s -resume <ID> \"...\"\n", runtimeName)
	return nil
}
