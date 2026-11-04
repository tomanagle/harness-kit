package agent

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Registry holds the runtimes available to a host. It is safe for
// concurrent use.
type Registry struct {
	discoverTimeout time.Duration
	mu              sync.RWMutex
	runtimes        map[string]Runtime
	order           []string
}

// NewRegistry returns a registry containing rts.
func NewRegistry(rts ...Runtime) *Registry {
	return NewRegistryWithOptions(RegistryOptions{}, rts...)
}

// RegistryOptions configures one registry independently of other hosts.
type RegistryOptions struct {
	// DiscoverTimeout bounds each probe. Non-positive values use 20 seconds.
	DiscoverTimeout time.Duration
}

// NewRegistryWithOptions returns a registry with instance-scoped discovery policy.
func NewRegistryWithOptions(options RegistryOptions, rts ...Runtime) *Registry {
	if options.DiscoverTimeout <= 0 {
		options.DiscoverTimeout = DiscoverTimeout
	}
	r := &Registry{runtimes: make(map[string]Runtime), discoverTimeout: options.DiscoverTimeout}
	for _, rt := range rts {
		if err := r.Register(rt); err != nil {
			panic(err)
		}
	}
	return r
}

// Register adds a runtime. Names must be unique and non-empty.
func (r *Registry) Register(rt Runtime) error {
	name := rt.Info().Name
	if name == "" {
		return fmt.Errorf("agent: runtime has empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.runtimes[name]; dup {
		return fmt.Errorf("agent: runtime %q already registered", name)
	}
	r.runtimes[name] = rt
	r.order = append(r.order, name)
	return nil
}

// Get returns the runtime registered under name.
func (r *Registry) Get(name string) (Runtime, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rt, ok := r.runtimes[name]
	return rt, ok
}

// List returns runtime metadata in registration order.
func (r *Registry) List() []Info {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Info, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.runtimes[name].Info())
	}
	return out
}

// DiscoverTimeout is the default per-runtime discovery timeout.
// Deprecated: configure each instance with RegistryOptions.DiscoverTimeout.
const DiscoverTimeout = 20 * time.Second

// Discover probes the named runtimes (all if none are given) concurrently.
// A runtime whose probe fails is reported with the failure in Problems
// rather than failing the whole call; the error is non-nil only for
// unknown names or when ctx ends.
func (r *Registry) Discover(ctx context.Context, names ...string) ([]Discovery, error) {
	r.mu.RLock()
	if len(names) == 0 {
		names = append([]string(nil), r.order...)
	}
	rts := make([]Runtime, len(names))
	for i, n := range names {
		rt, ok := r.runtimes[n]
		if !ok {
			r.mu.RUnlock()
			return nil, fmt.Errorf("agent: unknown runtime %q", n)
		}
		rts[i] = rt
	}
	r.mu.RUnlock()

	out := make([]Discovery, len(rts))
	var wg sync.WaitGroup
	for i, rt := range rts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			info := rt.Info()
			dctx, cancel := context.WithTimeout(ctx, r.discoverTimeout)
			defer cancel()
			d, err := rt.Discover(dctx)
			if d.Runtime == "" {
				d.Runtime = info.Name
			}
			if err != nil {
				d.Problems = append(d.Problems, err.Error())
				d.Ready = false
			}
			if d.Auth.State == "" {
				d.Auth.State = AuthUnknown
			}
			out[i] = d
		}()
	}
	wg.Wait()
	return out, ctx.Err()
}

// ListSessions lists the stored sessions of the named runtime. It returns
// an error wrapping ErrUnsupported if the runtime does not implement
// SessionLister.
func (r *Registry) ListSessions(ctx context.Context, name string, opts ListSessionsOptions) ([]RuntimeSession, error) {
	rt, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("agent: unknown runtime %q", name)
	}
	l, ok := rt.(SessionLister)
	if !ok {
		return nil, fmt.Errorf("%w: %s cannot list its sessions", ErrUnsupported, name)
	}
	return l.ListSessions(ctx, opts)
}
