package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
)

func (a app) startSession(args []string, name string) (*turnRunner, []string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	sf := &sessionFlags{}
	sf.version = a.version
	sf.register(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return nil, nil, err
	}
	if err := sf.prepare(fs); err != nil {
		return nil, nil, err
	}
	ctx := context.Background()
	b, err := sf.open(ctx)
	if err != nil {
		return nil, nil, err
	}
	var info session.Info
	restoring := isSessionID(sf.resume)
	if restoring {
		info, err = b.Restore(ctx, sf.resume)
		if errors.Is(err, session.ErrNotFound) || isReason(err, protocol.ReasonSessionNotFound) {
			err = fmt.Errorf("no session %s in %s (see harness-kit sessions)", sf.resume, sf.store)
		}
		if err == nil && sf.sessionSettings != nil {
			_, err = b.ConfigureSettings(ctx, settings.ConfigureParams{Target: settings.Target{SessionID: info.ID}, Patch: sf.sessionSettings})
			if err == nil {
				info, err = b.Get(ctx, info.ID)
			}
		}
		if err == nil && sf.title != "" {
			info, err = b.Rename(ctx, info.ID, sf.title)
		}
		if err == nil {
			var cfg agent.ConfigPatch
			changed := false
			fs.Visit(func(f *flag.Flag) {
				switch f.Name {
				case "model":
					cfg.Model = &sf.model
					changed = true
				case "effort":
					cfg.Effort = &sf.effort
					changed = true
				}
			})
			if changed {
				info, err = b.Configure(ctx, info.ID, cfg)
			}
		}
	} else {
		var p protocol.SessionCreateParams
		if p, err = sf.createParams(); err == nil {
			info, err = b.Create(ctx, p)
		}
	}
	if err != nil {
		b.Close()
		return nil, nil, err
	}
	mode := "in-process"
	if sf.rpc {
		mode = "via JSON-RPC sidecar"
	}
	if sf.out.format == "text" {
		if restoring {
			fmt.Fprintf(os.Stderr, "restored %s: %q, %d turn(s) so far (%s %s, %s)\n", info.ID, info.Title, info.Turns, info.Runtime, info.RuntimeSessionID, mode)
		} else {
			fmt.Fprintf(os.Stderr, "%s session %s (%s)\n", info.Runtime, info.RuntimeSessionID, mode)
		}
	}
	// Startup notes (MCP warnings, skipped servers) are session events.
	if evs, err := b.Events(ctx, info.ID, 0); err == nil && sf.out.format == "text" {
		for _, ev := range evs {
			if d, ok := ev.Data.(event.RuntimeStatusData); ok && (d.Status == "warning" || d.Status == "mcp") {
				fmt.Fprintf(os.Stderr, "[%s] %s\n", d.Status, d.Message)
			}
		}
	}
	r := &turnRunner{b: b, sf: sf, con: &console{}, info: info, textOut: os.Stdout, sigs: make(chan os.Signal, 1), closeOut: func() {}}
	if sf.out.format == "stream-json" || (name == "chat" && sf.out.format == "json") {
		r.textOut, r.closeOut = jsonWriter() // a stream of JSON lines
	}
	signal.Notify(r.sigs, os.Interrupt)
	return r, pos, nil
}

func (r *turnRunner) finish() {
	defer r.closeOut()
	signal.Stop(r.sigs)
	if info, err := r.b.Get(context.Background(), r.info.ID); err == nil && info.Turns > 0 && r.sf.out.format == "text" {
		if r.sf.persistent() {
			fmt.Fprintf(os.Stderr, "resume with: harness-kit prompt -resume %s \"...\"\n", info.ID)
		} else if info.RuntimeSessionID != "" {
			fmt.Fprintf(os.Stderr, "resume with: harness-kit prompt -runtime %s -resume %s \"...\"\n", info.Runtime, info.RuntimeSessionID)
		}
	}
	r.b.Close()
}

func (a app) promptCmd(args []string) error {
	r, words, err := a.startSession(args, "prompt")
	if err != nil {
		return err
	}
	defer r.finish()

	text := strings.Join(words, " ")
	r.canAsk = stdinIsTerminal()
	if text == "" && len(r.sf.images) == 0 {
		if r.canAsk {
			return errors.New(`usage: harness-kit prompt [flags] "your prompt" (or pipe the prompt on stdin)`)
		}
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		text, r.canAsk = strings.TrimSpace(string(b)), false
	}
	res, err := r.run(agent.Text(text))
	if err != nil {
		return err
	}
	if r.sf.out.format == "json" {
		if err := printJSON(res); err != nil {
			return err
		}
	}
	switch res.Status {
	case event.TurnStatusFailed:
		return exitCode(1)
	case event.TurnStatusCancelled:
		return exitCode(130)
	}
	return nil
}

func (a app) chatCmd(args []string) error {
	r, words, err := a.startSession(args, "chat")
	if err != nil {
		return err
	}
	defer r.finish()
	r.canAsk = stdinIsTerminal()
	turn := func(text string) error {
		res, err := r.run(agent.Text(text))
		if err == nil && r.sf.out.format == "json" {
			b, _ := json.Marshal(res) // one line per turn
			fmt.Fprintln(r.textOut, string(b))
		}
		return err
	}
	if len(words) > 0 {
		if err := turn(strings.Join(words, " ")); err != nil {
			return err
		}
	}
	if r.sf.out.format == "text" {
		fmt.Fprintln(os.Stderr, "type a message, /configure JSON to change defaults, /quit to exit; Ctrl-C cancels a running turn")
	}
	for {
		fmt.Fprint(os.Stderr, "\n> ")
		quit := make(chan struct{})
		lineC := make(chan string, 1)
		okC := make(chan bool, 1)
		go func() {
			l, ok := r.con.next(quit)
			lineC <- l
			okC <- ok
		}()
		select {
		case <-r.sigs:
			close(quit)
			fmt.Fprintln(os.Stderr)
			return nil
		case line := <-lineC:
			if !<-okC || line == "/quit" || line == "/exit" {
				return nil
			}
			if line == "" {
				continue
			}
			if line == "/configure" || strings.HasPrefix(line, "/configure ") {
				patch, err := parseConfigPatch(strings.TrimSpace(strings.TrimPrefix(line, "/configure")))
				if err == nil {
					var info session.Info
					info, err = r.b.Configure(context.Background(), r.info.ID, patch)
					if err == nil {
						r.info = info
					}
				}
				if err != nil {
					fmt.Fprintln(os.Stderr, "error:", err)
				} else {
					fmt.Fprintln(os.Stderr, "session defaults updated")
				}
				continue
			}
			if err := turn(line); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
		}
	}
}
