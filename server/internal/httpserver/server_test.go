package httpserver_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/server/internal/httpserver"
)

func TestShutdownDrainsInFlightRequests(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "finished")
	})
	server, err := httpserver.New(httpserver.Options{Config: httpserver.Defaults(), Handler: handler, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := httpserver.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	response := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 3 * time.Second}
		res, err := client.Get("http://" + listener.Addr().String())
		if err == nil {
			defer res.Body.Close()
			var body []byte
			body, err = io.ReadAll(res.Body)
			if err == nil && string(body) != "finished" {
				err = errors.New("response was not drained")
			}
		}
		response <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not arrive")
	}
	cancel()
	// An active request must keep shutdown pending until the handler completes.
	select {
	case err := <-done:
		t.Fatalf("shutdown abandoned request: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-response; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}

func TestShutdownTimeoutClosesActiveConnections(t *testing.T) {
	entered, finished := make(chan struct{}), make(chan struct{})
	cfg := httpserver.Defaults()
	cfg.ShutdownTimeout = 20 * time.Millisecond
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(finished) })
	server, err := httpserver.New(httpserver.Options{Config: cfg, Handler: handler, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := httpserver.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	requested := make(chan struct{})
	go func() {
		defer close(requested)
		client := &http.Client{Timeout: 3 * time.Second}
		res, err := client.Get("http://" + listener.Addr().String())
		if err == nil {
			res.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not arrive")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected shutdown timeout, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not return")
	}
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("active connection leaked")
	}
	<-requested
}
