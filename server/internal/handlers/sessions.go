package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/tomanagle/harness-kit/server/internal/store"
	"github.com/tomanagle/harness-kit/server/internal/wire"
)

func (h *handler) sessions(mux *http.ServeMux) {
	route(mux, h, "sync/get", true, h.store.GetSession)
	route(mux, h, "execution/acquire", true, h.store.AcquireLease)
	route(mux, h, "execution/renew", true, h.store.RenewLease)
	route(mux, h, "execution/release", true, h.store.ReleaseLease)
	route(mux, h, "execution/send", true, h.store.SendCommand)
	route(mux, h, "execution/claim", true, h.store.ClaimCommand)
	route(mux, h, "execution/complete", true, h.store.CompleteCommand)
	route(mux, h, "execution/get", true, h.store.GetCommand)
	route(mux, h, "sync/update", true, h.store.UpdateSession)
	route(mux, h, "sync/delete", true, h.store.DeleteSession)
	route(mux, h, "sync/push", true, h.store.Push)
	route(mux, h, "sync/events", true, h.store.Events)
	route(mux, h, "sync/changes", true, func(ctx context.Context, p store.Principal, in wire.ChangesRequest) (wire.Changes, error) {
		if in.WaitMs < 0 || in.WaitMs > 25000 {
			return wire.Changes{}, store.ErrInvalid
		}
		deadline := time.NewTimer(time.Duration(in.WaitMs) * time.Millisecond)
		defer deadline.Stop()
		for {
			out, err := h.store.Changes(ctx, p, in)
			if err != nil || len(out.Changes) > 0 || out.Cursor != in.After || in.WaitMs == 0 {
				return out, err
			}
			timer := time.NewTimer(250 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return out, ctx.Err()
			case <-deadline.C:
				timer.Stop()
				return out, nil
			case <-timer.C:
			}
		}
	})
}
