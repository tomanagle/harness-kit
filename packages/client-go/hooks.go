package harnesskit

import (
	"context"
	"fmt"
)

// HookFunc receives a cancellable lifecycle invocation. Return an empty response
// to preserve input. After-turn handlers cannot modify the completed result.
type HookFunc func(context.Context, HookRequest) (HookResponse, error)

// RegisterHook installs a handler before telling the host to invoke it. IDs are
// unique across connections; unregistering another owner's ID is rejected.
func (c *Client) RegisterHook(ctx context.Context, spec HookSpec, handler HookFunc) error {
	if handler == nil {
		return fmt.Errorf("hook handler is required")
	}
	c.hookOps.Lock()
	defer c.hookOps.Unlock()
	c.hookMu.Lock()
	if c.hooks == nil {
		c.hooks = map[string]HookFunc{}
	}
	if c.hooks[spec.ID] != nil {
		c.hookMu.Unlock()
		return fmt.Errorf("hook %s is already registered", spec.ID)
	}
	c.hooks[spec.ID] = handler
	c.hookMu.Unlock()
	if _, err := c.HooksRegister(ctx, HooksRegisterParams{Hook: spec}); err != nil {
		c.hookMu.Lock()
		delete(c.hooks, spec.ID)
		c.hookMu.Unlock()
		return err
	}
	return nil
}
func (c *Client) UnregisterHook(ctx context.Context, id string) error {
	c.hookOps.Lock()
	defer c.hookOps.Unlock()
	if _, err := c.HooksUnregister(ctx, HooksUnregisterParams{ID: id}); err != nil {
		return err
	}
	c.hookMu.Lock()
	delete(c.hooks, id)
	c.hookMu.Unlock()
	return nil
}
