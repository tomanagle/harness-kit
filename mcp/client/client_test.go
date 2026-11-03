package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/tomanagle/harness-kit/mcp"
)

// The test binary doubles as a stdio MCP server.
func TestMain(m *testing.M) {
	if os.Getenv("HK_FAKE_MCP") == "1" {
		fakeServer(os.Stdin, os.Stdout)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func handle(m map[string]any) (any, bool) {
	switch m["method"] {
	case "initialize":
		return map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "fake-mcp", "version": "1.2.3"}}, true
	case "tools/list":
		params, _ := m["params"].(map[string]any)
		if params["cursor"] == "page2" {
			return map[string]any{"tools": []any{map[string]any{"name": "b", "inputSchema": map[string]any{"type": "object"}}}}, true
		}
		return map[string]any{"tools": []any{map[string]any{"name": "a", "description": "tool a"}}, "nextCursor": "page2"}, true
	}
	return nil, false
}

func fakeServer(in io.Reader, out io.Writer) {
	if os.Getenv("HK_FAKE_MCP_TOKEN") != "t0k" {
		fmt.Fprintln(os.Stderr, "fake mcp: missing token")
		os.Exit(3)
	}
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil || m["id"] == nil {
			continue
		}
		if r, ok := handle(m); ok {
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": r})
			fmt.Fprintln(out, string(b))
		}
	}
}

func check(t *testing.T, res Result, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if res.ServerName != "fake-mcp" || res.ServerVersion != "1.2.3" || len(res.Tools) != 2 || res.Tools[0].Name != "a" || res.Tools[1].Name != "b" {
		t.Fatalf("result: %+v", res)
	}
}

func TestStdio(t *testing.T) {
	exe, _ := os.Executable()
	s := mcp.Server{Command: exe, Env: map[string]string{"HK_FAKE_MCP": "1", "HK_FAKE_MCP_TOKEN": "${TOK}", "GORACE": "atexit_sleep_ms=0"}}
	lookup := func(k string) (string, bool) {
		if k == "TOK" {
			return "t0k", true
		}
		return "", false
	}
	res, err := Probe(context.Background(), s, Options{Lookup: lookup})
	check(t, res, err)

	// A server that exits early reports its stderr.
	_, err = Probe(context.Background(), mcp.Server{Command: exe, Env: map[string]string{"HK_FAKE_MCP": "1", "GORACE": "atexit_sleep_ms=0"}}, Options{})
	if err == nil || !strings.Contains(err.Error(), "missing token") {
		t.Fatalf("early exit: %v", err)
	}
	if _, err := Probe(context.Background(), mcp.Server{Command: "x", Env: map[string]string{"K": "${UNSET_HK_VAR}"}}, Options{}); err == nil {
		t.Fatal("unset variable should fail before starting")
	}
}

func TestStreamableHTTP(t *testing.T) {
	for _, sse := range []bool{false, true} {
		var sawSession, sawAuth bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				return
			}
			sawAuth = r.Header.Get("Authorization") == "Bearer t0k"
			var m map[string]any
			json.NewDecoder(r.Body).Decode(&m)
			if m["method"] != "initialize" && r.Header.Get("Mcp-Session-Id") == "sess-1" {
				sawSession = true
			}
			w.Header().Set("Mcp-Session-Id", "sess-1")
			if m["id"] == nil {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			result, _ := handle(m)
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": result})
			if sse {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
		}))
		s := mcp.Server{Type: "http", URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer ${TOK}"}}
		res, err := Probe(context.Background(), s, Options{Lookup: func(k string) (string, bool) { return "t0k", k == "TOK" }})
		check(t, res, err)
		if !sawSession || !sawAuth {
			t.Fatalf("sse=%v: session header %v, auth %v", sse, sawSession, sawAuth)
		}
		srv.Close()
	}

	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer errSrv.Close()
	if _, err := Probe(context.Background(), mcp.Server{Type: "http", URL: errSrv.URL}, Options{}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("http error: %v", err)
	}
}
