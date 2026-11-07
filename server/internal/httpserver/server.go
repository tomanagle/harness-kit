// Package httpserver owns listener setup and graceful HTTP shutdown. Application
// routing and process signals are supplied by the composition root.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

type Config struct {
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	MaxHeaderBytes    int
}

func Defaults() Config {
	return Config{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second,
		ShutdownTimeout: 10 * time.Second, MaxHeaderBytes: 16384}
}

type Options struct {
	Config  Config
	Handler http.Handler
	Logger  *slog.Logger
}

type Server struct {
	http            *http.Server
	logger          *slog.Logger
	shutdownTimeout time.Duration
}

func New(opts Options) (*Server, error) {
	if opts.Handler == nil || opts.Logger == nil {
		return nil, errors.New("httpserver: handler and logger are required")
	}
	c := opts.Config
	if c.ReadHeaderTimeout <= 0 || c.ReadTimeout <= 0 || c.WriteTimeout <= 0 || c.IdleTimeout <= 0 || c.ShutdownTimeout <= 0 || c.MaxHeaderBytes < 1 {
		return nil, errors.New("httpserver: timeouts and header limit must be positive")
	}
	return &Server{http: &http.Server{Handler: opts.Handler, ReadHeaderTimeout: c.ReadHeaderTimeout,
		ReadTimeout: c.ReadTimeout, WriteTimeout: c.WriteTimeout, IdleTimeout: c.IdleTimeout,
		MaxHeaderBytes: c.MaxHeaderBytes, ErrorLog: slog.NewLogLogger(opts.Logger.Handler(), slog.LevelError)},
		logger: opts.Logger, shutdownTimeout: c.ShutdownTimeout}, nil
}

// MustNew is the fail-fast startup variant of New. Call it only from main.
func MustNew(opts Options) *Server {
	server, err := New(opts)
	if err != nil {
		panic(err)
	}
	return server
}

func Listen(address string) (net.Listener, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("httpserver: listen: %w", err)
	}
	return listener, nil
}

// MustListen is the fail-fast startup variant of Listen. Call it only from main.
func MustListen(address string) net.Listener {
	listener, err := Listen(address)
	if err != nil {
		panic(err)
	}
	return listener
}

// Serve owns the listener. Cancellation drains active requests; an expired
// shutdown deadline force-closes connections before returning the error.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	if listener == nil {
		return errors.New("httpserver: listener is required")
	}
	defer listener.Close()
	defer s.http.Close()
	if ctx.Err() != nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- s.http.Serve(listener) }()
	s.logger.Info("server listening", "address", listener.Addr().String())
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()
	err := s.http.Shutdown(shutdown)
	if err != nil {
		_ = s.http.Close()
	}
	serveErr := <-done
	if err != nil {
		return fmt.Errorf("httpserver: shutdown: %w", err)
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}
