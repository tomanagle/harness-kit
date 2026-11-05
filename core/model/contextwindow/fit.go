package contextwindow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/tomanagle/harness-kit/core/model"
)

var (
	ErrBudget   = errors.New("contextwindow: protected history exceeds token budget")
	ErrConflict = errors.New("contextwindow: history revision changed")
)

// Config can be stored in the host's shared settings document. Zero MaxTokens
// disables trimming. Output reservation includes at least Request.MaxOutputTokens.
type Config struct {
	MaxTokens            int64 `json:"maxTokens"`
	ReservedOutputTokens int64 `json:"reservedOutputTokens"`
	// KeepRecentTurns protects this many newest user turns. At least the most
	// recent turn is always kept, including its entire tool exchange.
	KeepRecentTurns int `json:"keepRecentTurns"`
}

func (c Config) Validate() error {
	if c.MaxTokens < 0 || c.ReservedOutputTokens < 0 || c.KeepRecentTurns < 0 {
		return errors.New("contextwindow: limits cannot be negative")
	}
	return nil
}

type Estimate struct {
	Tokens int64 `json:"tokens"`
	Exact  bool  `json:"exact"`
}

// Estimator accounts for the complete request, including tool definitions,
// images, instructions and native reasoning. Exact must be false for heuristics.
type Estimator func(context.Context, model.Request) (Estimate, error)

// Compactor is an optional host strategy. It may call a model or summarize by
// other means. It must preserve instructions and the protected recent turns.
type Compactor func(context.Context, model.Request, int64) ([]model.Message, error)

type Prepared struct {
	Request         model.Request `json:"request"`
	Before          Estimate      `json:"before"`
	After           Estimate      `json:"after"`
	DroppedMessages int           `json:"droppedMessages"`
	Compacted       bool          `json:"compacted"`
}

// Fit produces an independent request. Without a compactor it drops oldest
// complete user turns. It never mutates the caller's history, silently truncates
// a tool exchange, drops instructions or removes the newest user turn.
func Fit(ctx context.Context, r model.Request, c Config, estimate Estimator, compact Compactor) (Prepared, error) {
	if err := c.Validate(); err != nil {
		return Prepared{}, err
	}
	if err := ctx.Err(); err != nil {
		return Prepared{}, err
	}
	if err := r.Validate(); err != nil {
		return Prepared{}, err
	}
	messages, err := model.CloneMessages(r.Messages)
	if err != nil {
		return Prepared{}, err
	}
	r.Messages = messages
	r.Settings = r.Settings.Clone()
	// Snapshot schemas too; the returned request is independent of UI edits.
	b, err := json.Marshal(r.Tools)
	if err != nil {
		return Prepared{}, err
	}
	r.Tools = nil
	if err = json.Unmarshal(b, &r.Tools); err != nil {
		return Prepared{}, err
	}
	out := Prepared{Request: r}
	if estimate == nil {
		if c.MaxTokens == 0 {
			return out, nil
		}
		return out, errors.New("contextwindow: an estimator is required for a token budget")
	}
	count := func(req model.Request) (Estimate, error) {
		v, err := estimate(ctx, req)
		if err == nil && v.Tokens < 0 {
			err = errors.New("contextwindow: estimator returned negative tokens")
		}
		return v, err
	}
	out.Before, err = count(r)
	out.After = out.Before
	if err != nil {
		return out, err
	}
	if c.MaxTokens == 0 {
		return out, nil
	}
	reserved := max(c.ReservedOutputTokens, r.MaxOutputTokens)
	if reserved >= c.MaxTokens {
		return out, ErrBudget
	}
	target := c.MaxTokens - reserved
	if out.Before.Tokens <= target {
		return out, nil
	}
	groups := turnGroups(messages)
	keep := max(1, c.KeepRecentTurns)
	protectedStart := 0
	if len(groups) > keep {
		protectedStart = groups[len(groups)-keep]
	}
	if compact != nil {
		// Callbacks may edit their input in place. Keep the protected baseline
		// independent so that those edits cannot change what we compare against.
		callbackRequest := r
		callbackRequest.Messages, err = model.CloneMessages(r.Messages)
		if err != nil {
			return out, err
		}
		callbackRequest.Settings = r.Settings.Clone()
		callbackRequest.Tools = nil
		if err = json.Unmarshal(b, &callbackRequest.Tools); err != nil {
			return out, err
		}
		candidate, e := compact(ctx, callbackRequest, target)
		if e != nil {
			return out, e
		}
		if !reflect.DeepEqual(instructions(candidate), instructions(messages)) {
			return out, errors.New("contextwindow: compactor changed protected instructions")
		}
		if !hasSuffix(candidate, messages[protectedStart:]) {
			return out, errors.New("contextwindow: compactor changed protected recent turns")
		}
		out.Request.Messages, e = model.CloneMessages(candidate)
		if e != nil {
			return out, e
		}
		if e = out.Request.Validate(); e != nil {
			return out, e
		}
		out.After, e = count(out.Request)
		if e != nil {
			return out, e
		}
		out.Compacted = true
		out.DroppedMessages = max(0, len(messages)-len(candidate))
		if out.After.Tokens > target {
			return out, ErrBudget
		}
		return out, nil
	}
	removed := make(map[int]bool)
	for g := 0; g < len(groups)-keep; g++ {
		end := len(messages)
		if g+1 < len(groups) {
			end = groups[g+1]
		}
		for i := groups[g]; i < end; i++ {
			if !isInstruction(messages[i]) {
				removed[i] = true
			}
		}
		out.Request.Messages = nil
		for i, m := range messages {
			if !removed[i] {
				out.Request.Messages = append(out.Request.Messages, m)
			}
		}
		if err := out.Request.Validate(); err != nil {
			return out, fmt.Errorf("contextwindow: invalid trimmed exchange: %w", err)
		}
		out.After, err = count(out.Request)
		if err != nil {
			return out, err
		}
		out.DroppedMessages = len(removed)
		if out.After.Tokens <= target {
			return out, nil
		}
	}
	return out, ErrBudget
}

func isInstruction(m model.Message) bool { return m.Role == model.System || m.Role == model.Developer }

func instructions(ms []model.Message) []model.Message {
	var out []model.Message
	for _, m := range ms {
		if isInstruction(m) {
			out = append(out, m)
		}
	}
	return out
}

func hasSuffix(all, tail []model.Message) bool {
	return len(all) >= len(tail) && reflect.DeepEqual(all[len(all)-len(tail):], tail)
}

func turnGroups(ms []model.Message) []int {
	var starts []int
	for i, m := range ms {
		if isInstruction(m) {
			continue
		}
		ordinaryUser := m.Role == model.User
		for _, p := range m.Content {
			if p.Type == "tool_result" {
				ordinaryUser = false
			}
		}
		if len(starts) == 0 || ordinaryUser {
			starts = append(starts, i)
		}
	}
	return starts
}
