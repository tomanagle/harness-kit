package acp

import (
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/tomanagle/harness-kit/internal/jsonrpc"
)

// FileSystem is an optional host filesystem implementation. Nil functions
// are not advertised. Callbacks must enforce path/approval policy themselves,
// including symlinks and execution-affecting files, and respect cancellation.
// The adapter never reads or writes agent-requested text files on its own.
type FileSystem struct {
	ReadTextFile  func(context.Context, ReadTextFileRequest) (string, error)
	WriteTextFile func(context.Context, WriteTextFileRequest) error
}

type ReadTextFileRequest struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Line      *int   `json:"line,omitempty"`
	Limit     *int   `json:"limit,omitempty"`
	// Root is the trusted session working directory, set by the adapter.
	Root string `json:"-"`
}
type WriteTextFileRequest struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Content   string `json:"content"`
	Root      string `json:"-"`
}

// TerminalHost supplies all ACP terminal operations. It must enforce its own
// command, environment and working-directory policy and bounded output. No
// shell implementation is included. ReleaseSession must release every terminal
// owned by this session, including ones the agent forgot to release.
type TerminalHost interface {
	Create(context.Context, CreateTerminalRequest) (CreateTerminalResponse, error)
	Output(context.Context, TerminalRequest) (TerminalOutput, error)
	WaitForExit(context.Context, TerminalRequest) (TerminalExitStatus, error)
	Kill(context.Context, TerminalRequest) error
	Release(context.Context, TerminalRequest) error
	ReleaseSession(context.Context, string) error
}
type CreateTerminalRequest struct {
	SessionID       string       `json:"sessionId"`
	Command         string       `json:"command"`
	Args            []string     `json:"args,omitempty"`
	Env             []NamedValue `json:"env,omitempty"`
	Cwd             string       `json:"cwd,omitempty"`
	OutputByteLimit *uint64      `json:"outputByteLimit,omitempty"`
	Root            string       `json:"-"`
}
type CreateTerminalResponse struct {
	TerminalID string `json:"terminalId"`
}
type TerminalRequest struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}
type TerminalExitStatus struct {
	ExitCode *int    `json:"exitCode"`
	Signal   *string `json:"signal"`
}
type TerminalOutput struct {
	Output     string              `json:"output"`
	Truncated  bool                `json:"truncated"`
	ExitStatus *TerminalExitStatus `json:"exitStatus,omitempty"`
}

func (s *Session) hostRequest(ctx context.Context, req *jsonrpc.Request) (any, error) {
	invalid := func(message string) (any, error) {
		return nil, jsonrpc.NewError(jsonrpc.CodeInvalidParams, message, nil)
	}
	switch req.Method {
	case "fs/read_text_file":
		if s.rt.fs.ReadTextFile == nil {
			break
		}
		var params ReadTextFileRequest
		if err := req.UnmarshalParams(&params); err != nil {
			return nil, err
		}
		if !s.matches(params.SessionID) || !filepath.IsAbs(params.Path) || (params.Line != nil && *params.Line < 1) || (params.Limit != nil && *params.Limit < 1) {
			return invalid("invalid session, absolute path, line or limit")
		}
		params.Root = s.cwd
		content, err := s.rt.fs.ReadTextFile(ctx, params)
		if err != nil {
			return nil, err
		}
		return map[string]any{"content": content}, nil
	case "fs/write_text_file":
		if s.rt.fs.WriteTextFile == nil {
			break
		}
		var params WriteTextFileRequest
		if err := req.UnmarshalParams(&params); err != nil {
			return nil, err
		}
		if !s.matches(params.SessionID) || !filepath.IsAbs(params.Path) {
			return invalid("invalid session or absolute path")
		}
		params.Root = s.cwd
		if err := s.rt.fs.WriteTextFile(ctx, params); err != nil {
			return nil, err
		}
		return map[string]any{}, nil
	case "terminal/create":
		if s.rt.terminal == nil {
			break
		}
		var params CreateTerminalRequest
		if err := req.UnmarshalParams(&params); err != nil {
			return nil, err
		}
		if !s.matches(params.SessionID) || params.Command == "" || (params.Cwd != "" && !filepath.IsAbs(params.Cwd)) {
			return invalid("invalid session, command or absolute cwd")
		}
		params.Root = s.cwd
		return s.rt.terminal.Create(ctx, params)
	case "terminal/output", "terminal/wait_for_exit", "terminal/kill", "terminal/release":
		if s.rt.terminal == nil {
			break
		}
		var params TerminalRequest
		if err := req.UnmarshalParams(&params); err != nil {
			return nil, err
		}
		if !s.matches(params.SessionID) || params.TerminalID == "" {
			return invalid("invalid session or terminalId")
		}
		switch req.Method {
		case "terminal/output":
			return s.rt.terminal.Output(ctx, params)
		case "terminal/wait_for_exit":
			return s.rt.terminal.WaitForExit(ctx, params)
		case "terminal/kill":
			if err := s.rt.terminal.Kill(ctx, params); err != nil {
				return nil, err
			}
		case "terminal/release":
			if err := s.rt.terminal.Release(ctx, params); err != nil {
				return nil, err
			}
		}
		return map[string]any{}, nil
	}
	return nil, jsonrpc.NewError(jsonrpc.CodeMethodNotFound, "client method not supported: "+req.Method, nil)
}

func (s *Session) matches(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return id != "" && id == s.id
}

func (s *Session) Handle(ctx context.Context, req *jsonrpc.Request) (any, error) {
	if req.IsNotification() {
		switch req.Method {
		case "session/update":
			s.update(req.Params)
		case "$/cancel_request":
			var params struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if req.UnmarshalParams(&params) == nil {
				s.mu.Lock()
				cancel := s.requests[string(params.RequestID)]
				s.mu.Unlock()
				if cancel != nil {
					cancel()
				}
			}
		default:
			s.raw(req.Method, req.Params)
		}
		return nil, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	s.requests[string(req.ID)] = cancel
	active := s.active
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.requests, string(req.ID)); s.mu.Unlock() }()
	if active != nil {
		stop := context.AfterFunc(active.ctx, cancel)
		defer stop()
	}
	if req.Method == "session/request_permission" {
		return s.permission(ctx, req, active)
	}
	return s.hostRequest(ctx, req)
}
