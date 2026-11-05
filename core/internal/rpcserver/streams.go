package rpcserver

import (
	"context"
	"errors"

	"github.com/tomanagle/harness-kit/core/internal/jsonrpc"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
)

func (c *conn) startEvents() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sub != nil {
		return
	}
	c.sub = c.srv.mgr.Subscribe(nil, 0)
	sub := c.sub
	changes, stop := c.srv.mgr.SubscribeSettings()
	c.stopSettings = stop
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for snapshot := range changes {
			if err := c.rpc.Notify(context.Background(), protocol.NotificationSettingsUpdated, snapshot); err != nil {
				if errors.Is(err, jsonrpc.ErrClosed) {
					return
				}
				c.srv.log.Warn("send settings", "err", err)
			}
		}
	}()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for ev := range sub.C {
			if err := c.rpc.Notify(context.Background(), protocol.NotificationEvent, ev); err != nil {
				if errors.Is(err, jsonrpc.ErrClosed) {
					return
				}
				c.srv.log.Warn("send event", "err", err)
			}
		}
	}()
}

func (c *conn) stopEvents() {
	c.stopLogs()
	c.mu.Lock()
	sub := c.sub
	stop := c.stopSettings
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
	if sub != nil {
		sub.Close()
	}
	c.wg.Wait()
}

func (c *conn) startLogs() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.logs != nil {
		return
	}
	sub := c.srv.mgr.Logs().Subscribe()
	done := make(chan struct{})
	c.logs, c.logsDone = sub, done
	go func() {
		defer close(done)
		for notification := range sub.C {
			// Never report a log-delivery error through the same log stream.
			if err := c.rpc.Notify(context.Background(), protocol.NotificationLog, notification); err != nil {
				return
			}
		}
	}()
}

func (c *conn) stopLogs() {
	c.mu.Lock()
	sub, done := c.logs, c.logsDone
	c.logs, c.logsDone = nil, nil
	c.mu.Unlock()
	if sub != nil {
		sub.Close()
		<-done
	}
}
