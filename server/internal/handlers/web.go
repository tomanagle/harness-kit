package handlers

import (
	"net/http"
	"strings"
	"time"

	"github.com/tomanagle/harness-kit/server/internal/store"
	"github.com/tomanagle/harness-kit/server/internal/templates"
	"github.com/tomanagle/harness-kit/server/internal/wire"
)

func (h *handler) web(mux *http.ServeMux) {
	mux.HandleFunc("GET /account/recover", func(w http.ResponseWriter, r *http.Request) { _ = templates.Recovery().Render(r.Context(), w) })
	mux.HandleFunc("POST /account/recover", func(w http.ResponseWriter, r *http.Request) {
		if !h.form(w, r) {
			return
		}
		_, err := h.store.RequestEmail(r.Context(), wire.RequestEmail{Email: r.FormValue("email"), Purpose: r.FormValue("purpose")})
		if err != nil {
			h.fail(w, err)
			return
		}
		_ = templates.AccountMessage("If the account exists, an email has been queued. Check your inbox.").Render(r.Context(), w)
	})
	mux.HandleFunc("GET /account/confirm", func(w http.ResponseWriter, r *http.Request) {
		_ = templates.Confirm(r.URL.Query().Get("token"), r.URL.Query().Get("purpose") == "reset").Render(r.Context(), w)
	})
	mux.HandleFunc("POST /account/confirm", func(w http.ResponseWriter, r *http.Request) {
		if !h.form(w, r) {
			return
		}
		_, err := h.passwordOperation(func() (wire.Account, error) {
			_, err := h.store.ConfirmEmail(r.Context(), wire.ConfirmEmail{Token: r.FormValue("token"), Password: r.FormValue("password")})
			return wire.Account{}, err
		})
		if err != nil {
			h.fail(w, err)
			return
		}
		_ = templates.AccountMessage("Account updated. Sign in again to continue.").Render(r.Context(), w)
	})
	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		if !h.form(w, r) {
			return
		}
		p, err := h.account(r)
		if err == nil {
			_, err = h.store.Logout(r.Context(), p, wire.Empty{})
		}
		if err != nil {
			h.fail(w, err)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "hk_account", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: strings.HasPrefix(h.publicURL, "https:"), SameSite: http.SameSiteStrictMode})
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/device", http.StatusSeeOther) })
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		_ = templates.Login(h.registration).Render(r.Context(), w)
	})
	for _, path := range []string{"/login", "/register"} {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			if !h.form(w, r) {
				return
			}
			if r.URL.Path == "/register" && !h.registration {
				h.fail(w, store.ErrForbidden)
				return
			}
			account, err := h.passwordOperation(func() (wire.Account, error) {
				if r.URL.Path == "/register" {
					return h.store.Register(r.Context(), r.FormValue("email"), r.FormValue("password"))
				}
				return h.store.Login(r.Context(), r.FormValue("email"), r.FormValue("password"))
			})
			if err != nil {
				h.fail(w, err)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "hk_account", Value: account.AccessToken, Path: "/", Secure: strings.HasPrefix(h.publicURL, "https:"), HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int((12 * time.Hour).Seconds())})
			http.Redirect(w, r, "/device", http.StatusSeeOther)
		})
	}
	mux.HandleFunc("GET /device", func(w http.ResponseWriter, r *http.Request) {
		p, err := h.account(r)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		code := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("code")))
		var info wire.DeviceInfo
		var tenants wire.Tenants
		if code != "" {
			info, err = h.store.Inspect(r.Context(), p, wire.DeviceInspect{UserCode: code})
			if err == nil {
				tenants, err = h.store.Tenants(r.Context(), p)
			}
			if err != nil {
				h.fail(w, err)
				return
			}
		}
		_ = templates.Device(code, info, tenants.Tenants).Render(r.Context(), w)
	})
	mux.HandleFunc("POST /device", func(w http.ResponseWriter, r *http.Request) {
		if !h.form(w, r) {
			return
		}
		p, err := h.account(r)
		if err == nil {
			err = h.store.Approve(r.Context(), p, wire.DeviceApprove{UserCode: r.FormValue("code"), TenantID: r.FormValue("tenant")})
		}
		if err != nil {
			h.fail(w, err)
			return
		}
		_ = templates.Approved().Render(r.Context(), w)
	})
}

func (h *handler) form(w http.ResponseWriter, r *http.Request) bool {
	// Browser mutations require an exact origin. APIs use bearer tokens, never cookies.
	if r.Header.Get("Origin") != h.publicURL {
		h.fail(w, store.ErrForbidden)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := r.ParseForm(); err != nil {
		h.fail(w, store.ErrInvalid)
		return false
	}
	return true
}

func (h *handler) account(r *http.Request) (store.Principal, error) {
	cookie, err := r.Cookie("hk_account")
	if err != nil {
		return store.Principal{}, store.ErrUnauthorized
	}
	p, err := h.store.Authenticate(r.Context(), cookie.Value)
	if err == nil && p.DeviceID != "" {
		err = store.ErrForbidden
	}
	return p, err
}
