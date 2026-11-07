package handlers

import (
	"context"
	"net/http"

	"github.com/tomanagle/harness-kit/server/internal/store"
	"github.com/tomanagle/harness-kit/server/internal/wire"
)

func (h *handler) devices(mux *http.ServeMux) {
	route(mux, h, "devices/begin", false, func(ctx context.Context, p store.Principal, in wire.DeviceBegin) (wire.DeviceAuthorization, error) {
		out, err := h.store.Begin(ctx, in)
		out.VerificationURI = h.publicURL + "/device"
		return out, err
	})
	route(mux, h, "devices/inspect", true, h.store.Inspect)
	route(mux, h, "devices/approve", true, func(ctx context.Context, p store.Principal, in wire.DeviceApprove) (wire.Empty, error) {
		return wire.Empty{}, h.store.Approve(ctx, p, in)
	})
	route(mux, h, "devices/poll", false, func(ctx context.Context, p store.Principal, in wire.DevicePoll) (wire.Grant, error) {
		return h.store.Poll(ctx, in)
	})
	route(mux, h, "devices/refresh", false, func(ctx context.Context, p store.Principal, in wire.Refresh) (wire.Grant, error) {
		return h.store.Refresh(ctx, in)
	})
	route(mux, h, "devices/revoke", true, func(ctx context.Context, p store.Principal, in wire.Revoke) (wire.Empty, error) {
		return wire.Empty{}, h.store.Revoke(ctx, p, in)
	})
}
