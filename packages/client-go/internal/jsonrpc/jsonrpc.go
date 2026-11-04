// Package jsonrpc implements a small bidirectional JSON-RPC 2.0 connection
// over newline-delimited JSON (one message per line).
//
// It is derived from core/internal/jsonrpc and kept local so the standalone
// client does not depend on the server module. Either side of a Conn can send
// requests and notifications. See the module's LICENSE for attribution.
//
// Concurrency model: incoming notifications are dispatched sequentially on the
// read goroutine, so they are observed in wire order. Incoming requests are
// dispatched on their own goroutine so a slow handler (for example one waiting
// for a user's approval) never blocks the read loop.
package jsonrpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

// Standard JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	// CodeRequestCancelled is returned when a request was cancelled by the
	// peer (via the cancel notification) or by connection shutdown.
	CodeRequestCancelled = -32800
)

// DefaultMaxMessageSize bounds a single inbound line.
const DefaultMaxMessageSize = 32 << 20

// ErrClosed is returned by Call and Notify after the connection has closed.
var ErrClosed = errors.New("jsonrpc: connection closed")

// Error is a JSON-RPC error object. Handlers may return *Error to control
// the code and data sent to the peer.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message) }

// NewError builds an *Error, marshalling data if non-nil.
func NewError(code int, message string, data any) *Error {
	e := &Error{Code: code, Message: message}
	if data != nil {
		if b, err := json.Marshal(data); err == nil {
			e.Data = b
		}
	}
	return e
}

// Request is an inbound request or notification.
type Request struct {
	// ID is nil for notifications.
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

// IsNotification reports whether the message expects no response.
func (r *Request) IsNotification() bool { return r.ID == nil }

// UnmarshalParams decodes the params into v, mapping failures to an
// invalid-params error suitable for returning from a handler.
func (r *Request) UnmarshalParams(v any) error {
	if len(r.Params) == 0 || bytes.Equal(r.Params, []byte("null")) {
		return nil
	}
	if err := json.Unmarshal(r.Params, v); err != nil {
		return NewError(CodeInvalidParams, "invalid params: "+err.Error(), nil)
	}
	return nil
}

// Handler handles inbound requests and notifications. For notifications the
// result is ignored.
type Handler interface {
	Handle(ctx context.Context, req *Request) (result any, err error)
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, req *Request) (any, error)

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, req *Request) (any, error) { return f(ctx, req) }

// Options configure a Conn.
type Options struct {
	// MaxMessageSize bounds inbound lines; longer lines are discarded and
	// answered with a parse error. Defaults to DefaultMaxMessageSize.
	MaxMessageSize int
	// OmitVersion omits the "jsonrpc":"2.0" member on outbound messages.
	// Some peers (the Codex app-server) document this "lite" framing.
	OmitVersion bool
	// CancelMethod, when non-empty, is the notification method used for
	// request cancellation. Inbound notifications with this method cancel
	// the matching in-flight request; outbound Calls whose context is
	// cancelled send it to the peer. Params are {"id": <request id>}.
	CancelMethod string
	// OrderKey, if set, returns an ordering key for an inbound request.
	// Requests sharing a non-empty key are handled one at a time, in
	// arrival order; requests with different or empty keys still run
	// concurrently. Use it for operations whose order matters, such as
	// "prompt then cancel" on one session.
	OrderKey func(*Request) string
	// Inline, if set, selects requests to handle synchronously on the read
	// goroutine: nothing after them is read until they complete. Use it
	// for short handshake requests that later, possibly pipelined,
	// requests depend on.
	Inline func(*Request) bool
	// OnError receives non-fatal protocol errors (malformed input, write
	// failures). Optional.
	OnError func(error)
}

type wireMessage struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type outRequest struct {
	JSONRPC string `json:"jsonrpc,omitempty"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type outResponse struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type response struct {
	result json.RawMessage
	err    *Error
}

// Conn is a bidirectional JSON-RPC connection.
type Conn struct {
	r       io.Reader
	w       io.Writer
	handler Handler
	opts    Options

	writeMu sync.Mutex
	nextID  atomic.Int64

	mu       sync.Mutex
	pending  map[string]chan response
	inflight map[string]context.CancelFunc
	tails    map[string]chan struct{} // OrderKey -> completion of the last queued request
	closed   bool

	done    chan struct{}
	closeMu sync.Once
	err     error
	wg      sync.WaitGroup
}

// NewConn creates a connection. Call Run to start reading.
func NewConn(r io.Reader, w io.Writer, h Handler, opts Options) *Conn {
	if opts.MaxMessageSize <= 0 {
		opts.MaxMessageSize = DefaultMaxMessageSize
	}
	if h == nil {
		h = HandlerFunc(func(context.Context, *Request) (any, error) {
			return nil, NewError(CodeMethodNotFound, "method not found", nil)
		})
	}
	return &Conn{
		r:        r,
		w:        w,
		handler:  h,
		opts:     opts,
		pending:  make(map[string]chan response),
		inflight: make(map[string]context.CancelFunc),
		tails:    make(map[string]chan struct{}),
		done:     make(chan struct{}),
	}
}

// Done is closed when the connection stops reading.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Close fails pending requests and cancels handlers. The transport owner must
// also close the reader/writer to interrupt blocked I/O.
func (c *Conn) Close() { c.shutdown(ErrClosed) }

// Err returns the error that ended the read loop (nil on clean EOF).
func (c *Conn) Err() error {
	<-c.done
	return c.err
}

// Run reads messages until EOF or a read error and returns nil on clean
// EOF. ctx is the parent of every handler context. Reads cannot be
// interrupted portably, so to stop Run early the owner closes the
// underlying reader. In-flight request handlers are cancelled and awaited
// before Run returns.
func (c *Conn) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	br := bufio.NewReaderSize(c.r, 64<<10)
	var err error
	for {
		var line []byte
		line, err = c.readLine(br)
		if err != nil {
			break
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		c.dispatch(ctx, line)
		select {
		case <-c.done:
			return c.Err()
		default:
		}
	}
	if errors.Is(err, io.EOF) {
		err = nil
	}
	cancel()
	c.shutdown(err)
	c.wg.Wait()
	return err
}

var errLineTooLong = errors.New("jsonrpc: message exceeds maximum size")

func (c *Conn) readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	tooLong := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(chunk) > c.opts.MaxMessageSize {
				tooLong = true
				buf = nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err == io.EOF && len(buf) > 0 && !tooLong {
				return bytes.TrimRight(buf, "\r\n"), nil
			}
			return nil, err
		}
		if tooLong {
			c.reportError(errLineTooLong)
			c.writeResponse(json.RawMessage("null"), nil, NewError(CodeParseError, errLineTooLong.Error(), nil))
			buf, tooLong = nil, false
			continue
		}
		return bytes.TrimRight(buf, "\r\n"), nil
	}
}

func (c *Conn) dispatch(ctx context.Context, line []byte) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		c.writeResponse(json.RawMessage("null"), nil, NewError(CodeInvalidRequest, "batch requests are not supported", nil))
		return
	}
	var msg wireMessage
	if err := json.Unmarshal(trimmed, &msg); err != nil {
		c.reportError(fmt.Errorf("jsonrpc: parse error: %w", err))
		c.writeResponse(json.RawMessage("null"), nil, NewError(CodeParseError, "parse error", nil))
		return
	}

	switch {
	case msg.Method != "":
		req := &Request{ID: msg.ID, Method: msg.Method, Params: msg.Params}
		if req.IsNotification() {
			if c.opts.CancelMethod != "" && req.Method == c.opts.CancelMethod {
				c.handleCancel(req)
				return
			}
			_, _ = c.handler.Handle(ctx, req)
			return
		}
		c.handleRequest(ctx, req)
	case msg.ID != nil:
		key := idKey(msg.ID)
		c.mu.Lock()
		ch, ok := c.pending[key]
		delete(c.pending, key)
		c.mu.Unlock()
		if !ok {
			c.reportError(fmt.Errorf("jsonrpc: response for unknown id %s", msg.ID))
			return
		}
		ch <- response{result: msg.Result, err: msg.Error}
	default:
		c.writeResponse(json.RawMessage("null"), nil, NewError(CodeInvalidRequest, "invalid request", nil))
	}
}

func (c *Conn) handleRequest(ctx context.Context, req *Request) {
	reqCtx, cancel := context.WithCancel(ctx)
	key := idKey(req.ID)
	// Ordering is decided here, on the read goroutine, so it reflects
	// arrival order.
	var prev, mine chan struct{}
	orderKey := ""
	if c.opts.OrderKey != nil {
		orderKey = c.opts.OrderKey(req)
	}
	c.mu.Lock()
	c.inflight[key] = cancel
	if orderKey != "" {
		prev = c.tails[orderKey]
		mine = make(chan struct{})
		c.tails[orderKey] = mine
	}
	c.mu.Unlock()

	run := func() {
		defer func() {
			c.mu.Lock()
			delete(c.inflight, key)
			if mine != nil {
				if c.tails[orderKey] == mine {
					delete(c.tails, orderKey)
				}
				close(mine)
			}
			c.mu.Unlock()
			cancel()
		}()
		if prev != nil {
			select {
			case <-prev:
			case <-reqCtx.Done():
			}
		}
		result, err := c.safeHandle(reqCtx, req)
		if err != nil {
			c.writeResponse(req.ID, nil, toError(err))
			return
		}
		raw, merr := json.Marshal(result)
		if merr != nil {
			c.writeResponse(req.ID, nil, NewError(CodeInternalError, "marshal result: "+merr.Error(), nil))
			return
		}
		c.writeResponse(req.ID, raw, nil)
	}
	if c.opts.Inline != nil && c.opts.Inline(req) {
		run()
		return
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		run()
	}()
}

func (c *Conn) safeHandle(ctx context.Context, req *Request) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = NewError(CodeInternalError, fmt.Sprintf("handler panic: %v", r), nil)
		}
	}()
	return c.handler.Handle(ctx, req)
}

func (c *Conn) handleCancel(req *Request) {
	var p struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.ID == nil {
		return
	}
	c.mu.Lock()
	cancel := c.inflight[idKey(p.ID)]
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func toError(err error) *Error {
	var rpcErr *Error
	if errors.As(err, &rpcErr) {
		return rpcErr
	}
	if errors.Is(err, context.Canceled) {
		return NewError(CodeRequestCancelled, "request cancelled", nil)
	}
	return NewError(CodeInternalError, err.Error(), nil)
}

func (c *Conn) version() string {
	if c.opts.OmitVersion {
		return ""
	}
	return "2.0"
}

func (c *Conn) writeResponse(id json.RawMessage, result json.RawMessage, rpcErr *Error) {
	resp := outResponse{JSONRPC: c.version(), ID: id, Error: rpcErr}
	if rpcErr == nil {
		if result == nil {
			result = json.RawMessage("null")
		}
		resp.Result = result
	}
	if err := c.write(resp); err != nil {
		c.reportError(err)
	}
}

func (c *Conn) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.w.Write(b)
	return err
}

// Call sends a request and waits for its response, decoding the result
// into result (which may be nil). If ctx is cancelled before the response
// arrives, Call returns ctx.Err() and, if Options.CancelMethod is set,
// notifies the peer.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id := c.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	ch := make(chan response, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.pending[key] = ch
	c.mu.Unlock()

	if err := c.write(outRequest{JSONRPC: c.version(), ID: &id, Method: method, Params: params}); err != nil {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		return fmt.Errorf("jsonrpc: write %s: %w", method, err)
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return ErrClosed
		}
		if resp.err != nil {
			return resp.err
		}
		if result != nil && len(resp.result) > 0 {
			if err := json.Unmarshal(resp.result, result); err != nil {
				return fmt.Errorf("jsonrpc: decode %s result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		if c.opts.CancelMethod != "" {
			_ = c.Notify(context.Background(), c.opts.CancelMethod, map[string]int64{"id": id})
		}
		return ctx.Err()
	case <-c.done:
		return ErrClosed
	}
}

// Notify sends a notification.
func (c *Conn) Notify(_ context.Context, method string, params any) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrClosed
	}
	return c.write(outRequest{JSONRPC: c.version(), Method: method, Params: params})
}

func (c *Conn) shutdown(err error) {
	c.closeMu.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.err = err
		for k, ch := range c.pending {
			close(ch)
			delete(c.pending, k)
		}
		for _, cancel := range c.inflight {
			cancel()
		}
		c.mu.Unlock()
		close(c.done)
	})
}

func (c *Conn) reportError(err error) {
	if c.opts.OnError != nil {
		c.opts.OnError(err)
	}
}

// idKey normalises an id for map lookup: numbers and strings are kept in
// their canonical JSON form.
func idKey(id json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, id); err != nil {
		return string(id)
	}
	return buf.String()
}
