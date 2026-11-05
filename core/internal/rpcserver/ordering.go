package rpcserver

import (
	"encoding/json"
	"strings"

	"github.com/tomanagle/harness-kit/core/internal/jsonrpc"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
	"github.com/tomanagle/harness-kit/core/settings"
)

// orderKey serialises the requests that change a session's state, so a
// client's "prompt, then cancel" or "prompt, prompt" is applied in the
// order it was sent. Git operations also use this queue, so pipelined stage
// and commit requests stay ordered. Discovery, approvals and non-Git reads run
// concurrently. Git commands are bounded by policy and request cancellation.
// Preserve arrival order for ordinary prompts while allowing a hook's recursive
// prompt to reach the manager's busy check instead of waiting on itself.
func (c *conn) orderKey(req *jsonrpc.Request) string {
	if req.Method == protocol.MethodSessionPrompt {
		var p protocol.SessionIDParams
		if json.Unmarshal(req.Params, &p) == nil && p.SessionID != "" {
			c.mu.Lock()
			inHook := c.hookCalls[p.SessionID] > 0
			c.mu.Unlock()
			if inHook {
				return ""
			}
			return "prompt:" + p.SessionID
		}
	}
	return orderKey(req)
}

func orderKey(req *jsonrpc.Request) string {
	if strings.HasPrefix(req.Method, "hooks.") {
		return "hooks"
	}
	if req.Method == protocol.MethodLogsSubscribe || req.Method == protocol.MethodLogsUnsubscribe {
		return "logs"
	}
	// Compact waits for native completion. Manager admission reserves the
	// session; keeping this request outside the queue lets cancel/close run.
	// Callers await the preceding turn and the compaction before prompting.
	if req.Method == protocol.MethodSessionCompact {
		return ""
	}
	if strings.HasPrefix(req.Method, "git.") {
		var p protocol.SessionIDParams
		if json.Unmarshal(req.Params, &p) == nil && p.SessionID != "" {
			return "session:" + p.SessionID
		}
	}
	if req.Method == protocol.MethodSettingsConfigure {
		var p settings.ConfigureParams
		if json.Unmarshal(req.Params, &p) == nil && p.Target.SessionID != "" {
			return "session:" + p.Target.SessionID
		}
		return "settings"
	}
	switch req.Method {
	case protocol.MethodSessionPrompt, protocol.MethodSessionCancel, protocol.MethodSessionClose,
		protocol.MethodSessionRestore, protocol.MethodSessionDelete, protocol.MethodSessionRename, protocol.MethodSessionConfigure,
		protocol.MethodSessionEvict, protocol.MethodSessionSuspend, protocol.MethodSessionPin,
		protocol.MethodSessionFork, protocol.MethodSessionArchive, protocol.MethodSessionTags:
		var p protocol.SessionIDParams
		if json.Unmarshal(req.Params, &p) == nil && p.SessionID != "" {
			return "session:" + p.SessionID
		}
	}
	return ""
}
