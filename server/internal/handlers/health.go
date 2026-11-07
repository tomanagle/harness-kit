package handlers

import (
	"context"
	"github.com/tomanagle/harness-kit/server/internal/wire"
	"net/http"
	"time"
)

func (h *handler) health(mux *http.ServeMux) {
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) { write(w, wire.Empty{}) })
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := h.store.Ping(ctx); err != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		write(w, wire.Empty{})
	})
}
