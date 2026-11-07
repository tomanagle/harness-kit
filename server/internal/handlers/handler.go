// Package handlers translates HTTP requests into injected application operations.
// It owns routing, not process logging, database connections, or HTTP listeners.
package handlers

import (
	"errors"
	"github.com/tomanagle/harness-kit/server/internal/auth"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/tomanagle/harness-kit/server/internal/store"
)

type Options struct {
	Store                 store.Store
	Logger                *slog.Logger
	PublicURL             string
	Registration          bool
	AuthConcurrency       int
	AuthRequestsPerMinute int
	TrustedProxies        []netip.Prefix
}

type handler struct {
	store        store.Store
	logger       *slog.Logger
	publicURL    string
	registration bool
	authSlots    chan struct{}
}

func New(opts Options) (http.Handler, error) {
	if opts.Store == nil || opts.Logger == nil || opts.PublicURL == "" || opts.AuthConcurrency < 1 {
		return nil, errors.New("handlers: store, logger, public URL, and positive authentication concurrency are required")
	}
	h := &handler{store: opts.Store, logger: opts.Logger, publicURL: opts.PublicURL, registration: opts.Registration, authSlots: make(chan struct{}, opts.AuthConcurrency)}
	mux := http.NewServeMux()
	h.health(mux)
	h.accounts(mux)
	h.devices(mux)
	h.sessions(mux)
	h.web(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if opts.AuthRequestsPerMinute > 0 && r.Method == http.MethodPost && (strings.HasPrefix(r.URL.Path, "/v1/auth/") || strings.HasPrefix(r.URL.Path, "/v1/devices/") || r.URL.Path == "/login" || r.URL.Path == "/register" || strings.HasPrefix(r.URL.Path, "/account/")) {
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			address, _ := netip.ParseAddr(host)
			for _, prefix := range opts.TrustedProxies {
				if prefix.Contains(address) {
					if forwarded, err := netip.ParseAddr(r.Header.Get("X-Real-IP")); err == nil {
						address = forwarded
					}
					break
				}
			}
			if err := h.store.RateLimit(r.Context(), auth.Digest(address.String()+":"+r.URL.Path), opts.AuthRequestsPerMinute); err != nil {
				w.Header().Set("Retry-After", "60")
				h.fail(w, err)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
		mux.ServeHTTP(w, r)
	}), nil
}

// MustNew is the fail-fast startup variant of New. Call it only from main.
func MustNew(opts Options) http.Handler {
	handler, err := New(opts)
	if err != nil {
		panic(err)
	}
	return handler
}
