package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// pair returns two connected Conns running on in-memory pipes.
func pair(t *testing.T, ha, hb Handler, opts Options) (*Conn, *Conn) {
	t.Helper()
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	a := NewConn(ar, aw, ha, opts)
	b := NewConn(br, bw, hb, opts)
	go a.Run(context.Background())
	go b.Run(context.Background())
	t.Cleanup(func() {
		aw.Close()
		bw.Close()
		<-a.Done()
		<-b.Done()
	})
	return a, b
}

func TestCallRoundTrip(t *testing.T) {
	echo := HandlerFunc(func(ctx context.Context, req *Request) (any, error) {
		switch req.Method {
		case "echo":
			var p map[string]any
			if err := req.UnmarshalParams(&p); err != nil {
				return nil, err
			}
			return p, nil
		case "fail":
			return nil, NewError(-32001, "nope", map[string]string{"reason": "x"})
		}
		return nil, NewError(CodeMethodNotFound, "method not found", nil)
	})
	a, _ := pair(t, nil, echo, Options{})

	var got map[string]any
	if err := a.Call(context.Background(), "echo", map[string]any{"hello": "world"}, &got); err != nil {
		t.Fatal(err)
	}
	if got["hello"] != "world" {
		t.Fatalf("got %v", got)
	}

	err := a.Call(context.Background(), "fail", nil, nil)
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32001 || !strings.Contains(string(rpcErr.Data), "reason") {
		t.Fatalf("unexpected error %v", err)
	}

	err = a.Call(context.Background(), "missing", nil, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeMethodNotFound {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestNotificationsAreOrdered(t *testing.T) {
	got := make(chan string, 100)
	h := HandlerFunc(func(ctx context.Context, req *Request) (any, error) {
		var p struct{ N string }
		_ = json.Unmarshal(req.Params, &p)
		got <- p.N
		return nil, nil
	})
	a, _ := pair(t, nil, h, Options{})
	for i := 0; i < 50; i++ {
		if err := a.Notify(context.Background(), "n", map[string]string{"N": string(rune('A' + i%26))}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 50; i++ {
		select {
		case n := <-got:
			if want := string(rune('A' + i%26)); n != want {
				t.Fatalf("notification %d = %s, want %s", i, n, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timeout")
		}
	}
}

func TestSlowRequestDoesNotBlockReader(t *testing.T) {
	release := make(chan struct{})
	h := HandlerFunc(func(ctx context.Context, req *Request) (any, error) {
		if req.Method == "slow" {
			<-release
			return "slow-done", nil
		}
		return "fast-done", nil
	})
	a, _ := pair(t, nil, h, Options{})

	slow := make(chan error, 1)
	go func() { slow <- a.Call(context.Background(), "slow", nil, nil) }()

	var res string
	if err := a.Call(context.Background(), "fast", nil, &res); err != nil || res != "fast-done" {
		t.Fatalf("fast call: %v %q", err, res)
	}
	close(release)
	if err := <-slow; err != nil {
		t.Fatal(err)
	}
}

func TestCancelRequest(t *testing.T) {
	started := make(chan struct{})
	h := HandlerFunc(func(ctx context.Context, req *Request) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	a, _ := pair(t, nil, h, Options{CancelMethod: "$/cancelRequest"})

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- a.Call(ctx, "block", nil, nil) }()
	<-started
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestCallAfterCloseAndPendingFail(t *testing.T) {
	block := make(chan struct{})
	h := HandlerFunc(func(ctx context.Context, req *Request) (any, error) {
		<-block
		return nil, nil
	})
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	a := NewConn(ar, aw, nil, Options{})
	b := NewConn(br, bw, h, Options{})
	go a.Run(context.Background())
	go b.Run(context.Background())

	errc := make(chan error, 1)
	go func() { errc <- a.Call(context.Background(), "x", nil, nil) }()
	time.Sleep(20 * time.Millisecond)
	bw.Close() // a sees EOF
	if err := <-errc; !errors.Is(err, ErrClosed) {
		t.Fatalf("pending call: got %v", err)
	}
	if err := a.Call(context.Background(), "x", nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("call after close: got %v", err)
	}
	close(block)
	aw.Close()
	<-b.Done()
}

func TestMalformedInputAndBatch(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := NewConn(inR, outW, nil, Options{})
	go c.Run(context.Background())
	sc := bufio.NewScanner(outR)

	readResp := func() wireMessage {
		t.Helper()
		if !sc.Scan() {
			t.Fatal("no response")
		}
		var m wireMessage
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	io.WriteString(inW, "{not json\n")
	if m := readResp(); m.Error == nil || m.Error.Code != CodeParseError || string(m.ID) != "null" {
		t.Fatalf("parse error response: %+v", m)
	}
	io.WriteString(inW, `[{"jsonrpc":"2.0","id":1,"method":"x"}]`+"\n")
	if m := readResp(); m.Error == nil || m.Error.Code != CodeInvalidRequest {
		t.Fatalf("batch response: %+v", m)
	}
	// Peers that omit the jsonrpc member are accepted.
	io.WriteString(inW, `{"id":7,"method":"x"}`+"\r\n")
	if m := readResp(); m.Error == nil || m.Error.Code != CodeMethodNotFound || string(m.ID) != "7" || m.JSONRPC != "2.0" {
		t.Fatalf("lite request response: %+v", m)
	}
	inW.Close()
	<-c.Done()
}

func TestMaxMessageSize(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := HandlerFunc(func(ctx context.Context, req *Request) (any, error) { return "ok", nil })
	c := NewConn(inR, outW, h, Options{MaxMessageSize: 64})
	go c.Run(context.Background())
	sc := bufio.NewScanner(outR)

	go func() {
		io.WriteString(inW, `{"id":1,"method":"x","params":"`+strings.Repeat("a", 200)+`"}`+"\n")
		io.WriteString(inW, `{"id":2,"method":"x"}`+"\n")
	}()
	var first, second wireMessage
	sc.Scan()
	json.Unmarshal(sc.Bytes(), &first)
	sc.Scan()
	json.Unmarshal(sc.Bytes(), &second)
	if first.Error == nil || first.Error.Code != CodeParseError {
		t.Fatalf("oversized: %+v", first)
	}
	if string(second.ID) != "2" || second.Error != nil {
		t.Fatalf("connection should survive oversized line: %+v", second)
	}
	inW.Close()
	<-c.Done()
}

func TestOmitVersion(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := NewConn(inR, outW, nil, Options{OmitVersion: true})
	go c.Run(context.Background())
	go c.Notify(context.Background(), "hello", map[string]int{"a": 1})
	sc := bufio.NewScanner(outR)
	sc.Scan()
	if strings.Contains(sc.Text(), "jsonrpc") {
		t.Fatalf("version should be omitted: %s", sc.Text())
	}
	inW.Close()
	<-c.Done()
}

func TestOrderKeySerialisesSameKey(t *testing.T) {
	var mu sync.Mutex
	var order []string
	h := HandlerFunc(func(ctx context.Context, req *Request) (any, error) {
		var p struct{ Key, N string }
		_ = json.Unmarshal(req.Params, &p)
		if p.N == "0" {
			time.Sleep(30 * time.Millisecond) // first request is slow
		}
		mu.Lock()
		order = append(order, p.Key+p.N)
		mu.Unlock()
		return nil, nil
	})
	opts := Options{OrderKey: func(r *Request) string {
		var p struct{ Key string }
		_ = json.Unmarshal(r.Params, &p)
		return p.Key
	}}
	a, _ := pair(t, nil, h, opts)
	var wg sync.WaitGroup
	for _, call := range []map[string]string{{"Key": "a", "N": "0"}, {"Key": "a", "N": "1"}, {"Key": "", "N": "x"}} {
		wg.Add(1)
		go func() { defer wg.Done(); a.Call(context.Background(), "m", call, nil) }()
		time.Sleep(5 * time.Millisecond) // fix arrival order
	}
	wg.Wait()
	// "a1" must wait for the slow "a0"; the unkeyed request overtakes both.
	if strings.Join(order, ",") != "x,a0,a1" {
		t.Fatalf("order = %v", order)
	}
}
