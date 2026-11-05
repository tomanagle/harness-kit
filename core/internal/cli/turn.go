package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/permission"
	"github.com/tomanagle/harness-kit/core/session"
)

// turnResult is the --output json object.
type turnResult struct {
	Budget           *event.BudgetExceededData `json:"budget,omitempty"`
	SessionID        string                    `json:"sessionId"`
	Runtime          string                    `json:"runtime"`
	RuntimeSessionID string                    `json:"runtimeSessionId,omitempty"`
	TurnID           string                    `json:"turnId"`
	Status           event.TurnStatus          `json:"status"`
	Text             string                    `json:"text"`
	Usage            *event.UsageData          `json:"usage,omitempty"`
	Error            *event.ErrorInfo          `json:"error,omitempty"`
	DurationMs       int64                     `json:"durationMs"`
}

// replyText assembles the agent's reply from message events, in order,
// ignoring subagent messages.
type replyText struct {
	order []string
	text  map[string]string
}

func (t *replyText) add(itemID, text string, replace bool) {
	if t.text == nil {
		t.text = map[string]string{}
	}
	if _, ok := t.text[itemID]; !ok {
		t.order = append(t.order, itemID)
	}
	if replace {
		t.text[itemID] = text
	} else {
		t.text[itemID] += text
	}
}

func (t *replyText) String() string {
	parts := make([]string, 0, len(t.order))
	for _, id := range t.order {
		if s := strings.TrimSpace(t.text[id]); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n")
}

type turnRunner struct {
	b        backend
	sf       *sessionFlags
	con      *console
	canAsk   bool
	sigs     chan os.Signal
	info     session.Info
	textOut  io.Writer
	closeOut func() // flushes textOut (stops jq)
}

// run executes one turn and returns its result.
func (r *turnRunner) run(input []agent.InputPart) (turnResult, error) {
	ctx := context.Background()
	for _, path := range r.sf.images {
		input = append(input, agent.InputPart{Type: "image", Path: path})
	}
	r.sf.images = nil
	start := time.Now()
	turnID := session.NewID("turn_")
	res := turnResult{SessionID: r.info.ID, Runtime: r.info.Runtime, TurnID: turnID}
	if err := r.b.Prompt(ctx, r.info.ID, turnID, input, r.sf.turnPatch, r.sf.turnSettings); err != nil {
		return res, err
	}
	var reply replyText
	streamAll := r.sf.out.format == "stream-json"
	done := make(chan struct{})
	defer close(done)

	var lastSeq uint64
	cancelled := false
	for {
		select {
		case <-r.sigs:
			if cancelled {
				fmt.Fprintln(os.Stderr, "\nforce quit")
				os.Exit(130)
			}
			cancelled = true
			fmt.Fprintln(os.Stderr, "\n[cancelling turn - press Ctrl-C again to force quit]")
			_ = r.b.Cancel(ctx, r.info.ID)
		case ev, ok := <-r.b.Stream():
			if !ok {
				return res, errors.New("connection closed")
			}
			if ev.SessionID != r.info.ID {
				continue
			}
			batch := []event.Event{ev}
			if ev.Type == event.StreamGap {
				missed, err := r.b.Events(ctx, r.info.ID, lastSeq)
				if err != nil {
					return res, err
				}
				batch = missed
			}
			for _, e := range batch {
				// stream-json also shows session-level events (session.started).
				mine := e.TurnID == turnID || (streamAll && e.TurnID == "")
				if !mine || e.Seq <= lastSeq {
					continue
				}
				lastSeq = e.Seq
				r.render(e)
				switch d := e.Data.(type) {
				case event.MessageDeltaData:
					if d.ParentCallID == "" {
						reply.add(d.ItemID, d.Text, false)
					}
				case event.MessageCompletedData:
					if d.ParentCallID == "" {
						reply.add(d.ItemID, d.Text, true)
					}
				case permission.Request:
					r.answer(d, done)
				case event.TurnCompletedData:
					res.Status, res.Usage, res.Error, res.Budget = d.Status, d.Usage, d.Error, d.Budget
					res.Text = reply.String()
					res.DurationMs = time.Since(start).Milliseconds()
					res.RuntimeSessionID = r.info.RuntimeSessionID
					if info, err := r.b.Get(ctx, r.info.ID); err == nil {
						res.RuntimeSessionID = info.RuntimeSessionID
					}
					return res, nil
				}
			}
		}
	}
}

func (r *turnRunner) answer(req permission.Request, turnDone <-chan struct{}) {
	respond := func(o permission.Outcome, msg string) {
		if !req.Supports(o) {
			o = permission.Deny
		}
		if err := r.b.Respond(context.Background(), req.ID, permission.Decision{Outcome: o, Message: msg}); err != nil {
			fmt.Fprintln(os.Stderr, "  respond:", err)
		}
	}
	mode := r.sf.approve
	if mode == "ask" && !r.canAsk {
		mode = "deny"
		fmt.Fprintln(os.Stderr, "  (no terminal to ask; denying - use --approve allow to auto-approve)")
	}
	switch mode {
	case "allow":
		fmt.Fprintln(os.Stderr, "  auto-approved (--approve allow)")
		respond(permission.Allow, "")
	case "deny":
		respond(permission.Deny, "denied by harness-kit CLI (--approve)")
	default:
		if r.sf.out.format != "text" { // text mode already showed the request
			fmt.Fprintf(os.Stderr, "\n⚠ %s wants to: %s\n", req.Runtime, req.Title)
		}
		go func() {
			fmt.Fprint(os.Stderr, "  allow? [y]es / [a]lways this session / [n]o / [q]uit turn: ")
			line, ok := r.con.next(turnDone)
			if !ok {
				return // turn ended (e.g. cancelled) before an answer
			}
			outcome := map[string]permission.Outcome{"y": permission.Allow, "yes": permission.Allow, "a": permission.AllowSession, "q": permission.Abort}[strings.ToLower(line)]
			if outcome == "" {
				outcome = permission.Deny
			}
			respond(outcome, "")
		}()
	}
}

func (r *turnRunner) render(ev event.Event) {
	switch r.sf.out.format {
	case "stream-json":
		b, _ := json.Marshal(ev)
		fmt.Fprintln(r.textOut, string(b))
		return
	case "json":
		return // one result object is printed when the turn ends
	}
	switch d := ev.Data.(type) {
	case event.MessageDeltaData:
		fmt.Fprint(r.textOut, d.Text)
	case event.ToolStartedData:
		fmt.Fprintf(os.Stderr, "\n· %s\n", firstNonEmpty(d.Title, d.Name))
	case event.ToolCompletedData:
		if d.Status != event.ToolStatusCompleted {
			fmt.Fprintf(os.Stderr, "· %s %s\n", d.Status, d.Error)
		}
	case permission.Request:
		fmt.Fprintf(os.Stderr, "\n⚠ %s wants to: %s\n", d.Runtime, d.Title)
		if d.Reason != "" {
			fmt.Fprintf(os.Stderr, "  reason: %s\n", d.Reason)
		}
	case event.ApprovalResolvedData:
		if d.Source != permission.SourceHost {
			fmt.Fprintf(os.Stderr, "· approval %s (%s)\n", d.Decision.Outcome, d.Source)
		}
	case event.RuntimeStatusData:
		if d.Status != "ready" {
			fmt.Fprintf(os.Stderr, "\n[%s] %s\n", d.Status, d.Message)
		}
	case event.ErrorData:
		fmt.Fprintf(os.Stderr, "\n[error] %s\n", d.Message)
	case event.TurnCompletedData:
		fmt.Fprintln(r.textOut)
		line := string(d.Status)
		if d.Usage != nil {
			line += fmt.Sprintf(" · %d in / %d out tokens", d.Usage.InputTokens, d.Usage.OutputTokens)
			if d.Usage.CostUSD != nil {
				line += fmt.Sprintf(" · ~$%.4f", *d.Usage.CostUSD)
			}
		}
		if d.Error != nil {
			line += " · " + d.Error.Message
		}
		fmt.Fprintln(os.Stderr, "—", line)
	}
}
