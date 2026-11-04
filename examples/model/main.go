// This example runs a host-owned tool loop. It makes paid API requests only when
// executed with an explicitly selected provider/model and configured credentials.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/tomanagle/harness-kit/core/config"
	"github.com/tomanagle/harness-kit/core/mcp"
	"github.com/tomanagle/harness-kit/core/model"
	"github.com/tomanagle/harness-kit/core/model/contextwindow"
	hkanthropic "github.com/tomanagle/harness-kit/core/providers/anthropic"
	hkopenai "github.com/tomanagle/harness-kit/core/providers/openai"
	"github.com/tomanagle/harness-kit/core/tools"
)

// AppSettings is the example application's single settings object. A real host
// can snapshot the same object from a UI/store before every inference request.
type AppSettings struct {
	Model           config.Settings      `json:"model"`
	Context         contextwindow.Config `json:"context"`
	MaxOutputTokens int64                `json:"maxOutputTokens"`
	MaxSteps        int                  `json:"maxSteps"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	var chosen, modelName, effort, prompt string
	settings := AppSettings{}
	flag.StringVar(&chosen, "provider", "", "openai or anthropic (required)")
	flag.StringVar(&modelName, "model", "", "provider model ID (required)")
	flag.StringVar(&effort, "effort", "", "provider-supported reasoning effort")
	flag.StringVar(&prompt, "prompt", "", "prompt (required)")
	flag.Int64Var(&settings.MaxOutputTokens, "max-output", 1024, "maximum output tokens per API request")
	flag.Int64Var(&settings.Context.MaxTokens, "context-tokens", 0, "estimated context limit; 0 disables trimming")
	flag.IntVar(&settings.MaxSteps, "max-steps", 8, "maximum API requests in this example")
	flag.Parse()
	if modelName == "" || prompt == "" {
		return errors.New("set -provider, -model and -prompt explicitly")
	}
	settings.Model = config.Settings{Model: modelName, Effort: effort}
	var provider model.Provider
	switch chosen {
	case "openai":
		provider = hkopenai.New()
	case "anthropic":
		provider = hkanthropic.New()
	default:
		return errors.New("-provider must be openai or anthropic")
	}
	registry := tools.NewRegistry()
	if err := tools.Add(registry, "current_time", "Return the current UTC time", func(ctx context.Context, in struct{}) (string, error) {
		return time.Now().UTC().Format(time.RFC3339), nil
	}, tools.ReadOnly()); err != nil {
		return err
	}
	history, err := contextwindow.New(model.Text(model.User, prompt))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runLoop(ctx, provider, registry, history, func() AppSettings { return settings }, os.Stdout)
}

// The host decides when to reload settings, compact, approve tools and stop.
// This example denies approval-required tools; applications can replace that
// branch with their own approval interface before Registry.Call.
func runLoop(ctx context.Context, p model.Provider, registry *tools.Registry, history *contextwindow.Window, current func() AppSettings, out io.Writer) error {
	for step := 0; ; step++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		s := current()
		if s.MaxSteps <= 0 || step >= s.MaxSteps {
			return errors.New("model example: request limit reached")
		}
		snapshot := history.Snapshot()
		req := model.Request{Settings: s.Model.Clone(), Messages: snapshot.Messages, MaxOutputTokens: s.MaxOutputTokens}
		for _, tool := range registry.List() {
			req.Tools = append(req.Tools, model.Tool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
		}
		prepared, err := contextwindow.Fit(ctx, req, s.Context, contextwindow.ApproximateJSON(4), nil)
		if err != nil {
			return err
		}
		if err = history.Replace(snapshot.Revision, prepared.Request.Messages); err != nil {
			return err
		}
		message, _, err := model.Collect(ctx, p, prepared.Request, func(e model.Event) bool {
			if e.Type == model.TextDelta {
				fmt.Fprint(out, e.Text)
			}
			return true
		})
		if err != nil {
			return err
		}
		if err = history.Append(message); err != nil {
			return err
		}
		calls := message.Calls()
		if len(calls) == 0 {
			fmt.Fprintln(out)
			return nil
		}
		for _, call := range calls {
			if err := ctx.Err(); err != nil {
				return err
			}
			tool, ok := registry.Get(call.Name)
			result := tools.ErrorResult("unknown tool")
			if ok {
				if tool.Approval == mcp.ApprovalAsk {
					result = tools.ErrorResult("host approval required; this example denies the call")
				} else {
					result, err = registry.Call(ctx, tools.Call{Name: call.Name, Arguments: call.Arguments})
					if err != nil {
						result = tools.ErrorResult(err.Error())
					}
				}
			}
			if err = history.Append(model.Result(call.ID, result.Text(), result.IsError)); err != nil {
				return err
			}
		}
	}
}
