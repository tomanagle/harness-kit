package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/tomanagle/harness-kit/server/internal/store"
	"github.com/tomanagle/harness-kit/server/internal/wire"
)

func (h *handler) passwordOperation(fn func() (wire.Account, error)) (wire.Account, error) {
	select {
	case h.authSlots <- struct{}{}:
		defer func() { <-h.authSlots }()
		return fn()
	default:
		return wire.Account{}, store.ErrSlowDown
	}
}

func route[I, O any](mux *http.ServeMux, h *handler, path string, authenticated bool, fn func(context.Context, store.Principal, I) (O, error)) {
	mux.HandleFunc("POST /v1/"+path, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" && r.Header.Get("Origin") != h.publicURL {
			h.fail(w, store.ErrForbidden)
			return
		}
		if media := strings.Split(r.Header.Get("Content-Type"), ";")[0]; media != "application/json" {
			h.fail(w, store.ErrInvalid)
			return
		}
		var p store.Principal
		var err error
		if authenticated {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			p, err = h.store.Authenticate(r.Context(), token)
			if err != nil {
				h.fail(w, err)
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1048576)
		d := json.NewDecoder(r.Body)
		d.DisallowUnknownFields()
		var in I
		if err = d.Decode(&in); err != nil {
			h.fail(w, store.ErrInvalid)
			return
		}
		if err = d.Decode(new(any)); err != io.EOF {
			h.fail(w, store.ErrInvalid)
			return
		}
		out, err := fn(r.Context(), p, in)
		if err != nil {
			h.fail(w, err)
			return
		}
		write(w, out)
	})
}

func write(w http.ResponseWriter, out any) {
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(out)
}

func (h *handler) fail(w http.ResponseWriter, err error) {
	status, code := 500, "internal_error"
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{{store.ErrGone, 410, "gone"}, {store.ErrInvalid, 400, "invalid_request"}, {store.ErrUnauthorized, 401, "unauthorized"}, {store.ErrForbidden, 403, "forbidden"}, {store.ErrNotFound, 404, "not_found"}, {store.ErrConflict, 409, "conflict"}, {store.ErrPending, 400, "authorization_pending"}, {store.ErrExpired, 400, "expired_token"}, {store.ErrSlowDown, 429, "slow_down"}} {
		if errors.Is(err, c.err) {
			status, code = c.status, c.code
			break
		}
	}
	if status == 500 {
		h.logger.Error("request failed", "error", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(wire.Error{Code: code, Message: code})
}
