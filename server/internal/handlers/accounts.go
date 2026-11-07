package handlers

import (
	"context"
	"net/http"

	"github.com/tomanagle/harness-kit/server/internal/store"
	"github.com/tomanagle/harness-kit/server/internal/wire"
)

func (h *handler) accounts(mux *http.ServeMux) {
	route(mux, h, "auth/request-email", false, func(ctx context.Context, _ store.Principal, in wire.RequestEmail) (wire.Empty, error) {
		return h.store.RequestEmail(ctx, in)
	})
	route(mux, h, "auth/confirm-email", false, func(ctx context.Context, _ store.Principal, in wire.ConfirmEmail) (wire.Empty, error) {
		var result wire.Empty
		_, err := h.passwordOperation(func() (wire.Account, error) {
			var e error
			result, e = h.store.ConfirmEmail(ctx, in)
			return wire.Account{}, e
		})
		return result, err
	})
	route(mux, h, "tenants/members", true, h.store.Members)
	route(mux, h, "tenants/set-role", true, h.store.SetMemberRole)
	route(mux, h, "devices/list", true, h.store.Devices)
	route(mux, h, "auth/logout", true, h.store.Logout)
	route(mux, h, "auth/register", false, func(ctx context.Context, p store.Principal, in wire.Credentials) (wire.Account, error) {
		if !h.registration {
			return wire.Account{}, store.ErrForbidden
		}
		return h.passwordOperation(func() (wire.Account, error) { return h.store.Register(ctx, in.Email, in.Password) })
	})
	route(mux, h, "auth/login", false, func(ctx context.Context, p store.Principal, in wire.Credentials) (wire.Account, error) {
		return h.passwordOperation(func() (wire.Account, error) { return h.store.Login(ctx, in.Email, in.Password) })
	})
	route(mux, h, "tenants/list", true, func(ctx context.Context, p store.Principal, in wire.Empty) (wire.Tenants, error) {
		return h.store.Tenants(ctx, p)
	})
	route(mux, h, "tenants/create", true, h.store.CreateTenant)
	route(mux, h, "tenants/add-member", true, func(ctx context.Context, p store.Principal, in wire.AddMember) (wire.Empty, error) {
		return wire.Empty{}, h.store.AddMember(ctx, p, in)
	})
	route(mux, h, "tenants/remove-member", true, func(ctx context.Context, p store.Principal, in wire.RemoveMember) (wire.Empty, error) {
		return wire.Empty{}, h.store.RemoveMember(ctx, p, in)
	})
}
