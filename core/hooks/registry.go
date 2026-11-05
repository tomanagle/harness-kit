package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
)

type entry struct {
	owner   string
	spec    Spec
	handler Handler
}

type Registry struct {
	active  int
	mu      sync.RWMutex
	entries map[string]entry
}

func NewRegistry() *Registry { return &Registry{entries: map[string]entry{}} }

func (r *Registry) Register(owner string, spec Spec, handler Handler) error {
	if owner == "" || spec.ID == "" || len(spec.ID) > 200 || !spec.Name.Valid() || handler == nil {
		return fmt.Errorf("invalid hook registration")
	}
	if spec.Name == BeforeCreate && spec.SessionID != "" {
		return fmt.Errorf("before-create hooks cannot target an existing session")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[spec.ID]; ok {
		return fmt.Errorf("hook %q is already registered", spec.ID)
	}
	r.entries[spec.ID] = entry{owner, spec, handler}
	return nil
}

func (r *Registry) Unregister(owner, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[id]; ok {
		if e.owner != owner {
			return fmt.Errorf("hook %q belongs to another owner", id)
		}
		delete(r.entries, id)
	}
	return nil
}

func (r *Registry) DropOwner(owner string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, e := range r.entries {
		if e.owner == owner {
			delete(r.entries, id)
		}
	}
}

func (r *Registry) List() []Spec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []Spec{}
	for _, e := range r.entries {
		out = append(out, e.spec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func clone[T any](in T) (T, error) {
	var out T
	raw, err := json.Marshal(in)
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	return out, err
}

func (r *Registry) Run(ctx context.Context, request Request, policy Settings) (Request, []string, error) {
	if !policy.Enabled {
		return request, nil, nil
	}
	if err := policy.Validate(); err != nil {
		return request, nil, err
	}
	r.mu.RLock()
	entries := []entry{}
	for _, id := range policy.Required {
		if _, ok := r.entries[id]; !ok {
			r.mu.RUnlock()
			return request, nil, fmt.Errorf("%w: required hook %q is not registered", ErrFailed, id)
		}
	}
	for _, e := range r.entries {
		if e.spec.Name == request.Name && (e.spec.SessionID == "" || request.Session != nil && e.spec.SessionID == request.Session.ID) {
			entries = append(entries, e)
		}
	}
	r.mu.RUnlock()
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].spec.Order != entries[j].spec.Order {
			return entries[i].spec.Order < entries[j].spec.Order
		}
		return entries[i].spec.ID < entries[j].spec.ID
	})
	request, err := clone(request)
	if err != nil {
		return request, nil, err
	}
	warnings := []string{}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return request, warnings, err
		}
		call, err := clone(request)
		if err != nil {
			return request, warnings, err
		}
		call.ConcurrencyLimit = policy.MaxConcurrent
		call.HookID = e.spec.ID
		call.InvocationID = request.InvocationID + ":" + e.spec.ID
		deadline, cancel := context.WithTimeout(ctx, time.Duration(policy.TimeoutMs)*time.Millisecond)
		response, err := r.invokeBounded(deadline, e.handler, call, policy.MaxConcurrent)
		if deadline.Err() != nil {
			err = deadline.Err()
		}
		cancel()
		if err == nil {
			response, err = clone(response)
		}
		if err == nil && response.Config != nil {
			_, err = response.Config.Fields()
		}
		if err == nil {
			switch request.Name {
			case BeforeCreate:
				if response.Input != nil || response.Config != nil {
					err = fmt.Errorf("before-create accepts only create")
				}
			case BeforeTurn:
				if response.Create != nil {
					err = fmt.Errorf("before-turn cannot change session identity")
				}
			case AfterTurn:
				if response.Create != nil || response.Input != nil || response.Config != nil {
					err = fmt.Errorf("after-turn cannot modify a completed turn")
				}
			}
		}
		if ctx.Err() != nil {
			return request, warnings, ctx.Err()
		}
		if err != nil {
			err = fmt.Errorf("%w: hook %s: %w", ErrFailed, e.spec.ID, err)
			if policy.OnError != "continue" {
				return request, warnings, err
			}
			warnings = append(warnings, err.Error())
			continue
		}
		if response.Create != nil {
			request.Create = response.Create
		}
		if response.Input != nil {
			request.Input = *response.Input
		}
		if response.Config != nil {
			request.Config = mergePatch(request.Config, response.Config)
		}
	}
	return request, warnings, nil
}

func invoke(ctx context.Context, h Handler, r Request) (out Response, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("handler panicked: %v", p)
		}
	}()
	return h.Handle(ctx, r)
}

// Go cannot forcibly stop arbitrary application code. Deadline expiry releases
// the caller; an uncooperative handler holds a bounded slot until it returns.
func (r *Registry) invokeBounded(ctx context.Context, h Handler, call Request, limit int) (Response, error) {
	r.mu.Lock()
	if r.active >= limit {
		r.mu.Unlock()
		return Response{}, fmt.Errorf("hook concurrency limit reached")
	}
	r.active++
	r.mu.Unlock()
	type result struct {
		response Response
		err      error
	}
	done := make(chan result, 1)
	go func() {
		defer func() { r.mu.Lock(); r.active--; r.mu.Unlock() }()
		response, err := invoke(ctx, h, call)
		done <- result{response, err}
	}()
	select {
	case <-ctx.Done():
		return Response{}, ctx.Err()
	case result := <-done:
		return result.response, result.err
	}
}

// Later handlers replace earlier edits to the same field, including resets.
func mergePatch(base, next *agent.ConfigPatch) *agent.ConfigPatch {
	if base == nil {
		base = &agent.ConfigPatch{}
	}
	clear := func(path string) {
		base.Reset = slices.DeleteFunc(base.Reset, func(p string) bool { return p == path })
		switch path {
		case "model":
			base.Model = nil
		case "effort":
			base.Effort = nil
		default:
			delete(base.Options, strings.TrimPrefix(path, "options."))
		}
	}
	for _, path := range next.Reset {
		clear(path)
		base.Reset = append(base.Reset, path)
	}
	if next.Model != nil {
		clear("model")
		base.Model = next.Model
	}
	if next.Effort != nil {
		clear("effort")
		base.Effort = next.Effort
	}
	for key, value := range next.Options {
		clear("options." + key)
		if base.Options == nil {
			base.Options = map[string]json.RawMessage{}
		}
		base.Options[key] = value
	}
	return base
}
