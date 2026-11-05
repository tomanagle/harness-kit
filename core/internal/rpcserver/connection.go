package rpcserver

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/internal/jsonrpc"
	"github.com/tomanagle/harness-kit/core/logging"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
	"github.com/tomanagle/harness-kit/core/tools"
)

type conn struct {
	hookCalls   map[string]int
	hookOwner   string
	srv         *Server
	rpc         *jsonrpc.Conn
	initialized atomic.Bool

	mu           sync.Mutex
	sub          *event.Subscription
	stopSettings func()
	logs         *logging.Subscription
	logsDone     chan struct{}
	wg           sync.WaitGroup
	tools        []string // names this client registered
}

// dropTools removes the client's tools when it disconnects.
func (c *conn) dropTools() {
	c.mu.Lock()
	names := c.tools
	c.tools = nil
	c.mu.Unlock()
	if reg := c.srv.opts.Tools; reg != nil && len(names) > 0 {
		reg.Unregister(names...)
	}
}

// forward returns a handler that runs the tool in the client.
func (c *conn) forward(name string) tools.Handler {
	return func(ctx context.Context, call tools.Call) (tools.Result, error) {
		var res tools.Result
		err := c.rpc.Call(ctx, protocol.RequestToolCall, protocol.ToolCallParams{
			SessionID: call.SessionID, TurnID: call.TurnID, Name: name, Arguments: call.Arguments,
		}, &res)
		if err != nil {
			return tools.Result{}, fmt.Errorf("the host application could not run %s: %v", name, err)
		}
		return res, nil
	}
}
