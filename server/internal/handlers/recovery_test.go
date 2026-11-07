package handlers_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	remote "github.com/tomanagle/harness-kit/packages/client-go/remote"
	"github.com/tomanagle/harness-kit/server/internal/auth"
	"github.com/tomanagle/harness-kit/server/internal/email"
	"github.com/tomanagle/harness-kit/server/internal/handlers"
	"github.com/tomanagle/harness-kit/server/internal/maintenance"
	"github.com/tomanagle/harness-kit/server/internal/store/dbstore"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type captureMail struct {
	mu       sync.Mutex
	messages []email.Message
	fail     bool
}

func (m *captureMail) Send(_ context.Context, msg email.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("smtp unavailable")
	}
	m.messages = append(m.messages, msg)
	return nil
}
func TestAccountRecoveryAndDistributedMail(t *testing.T) {
	pool, owner := database(t)
	ctx := context.Background()
	passwords, err := auth.NewPasswords(rand.Reader)
	requireNoError(t, err)
	tokens, err := auth.NewTokens(rand.Reader)
	requireNoError(t, err)
	repo, err := dbstore.New(dbstore.Options{Database: pool, Passwords: passwords, Tokens: tokens, Now: time.Now, MailEnabled: true, RequireVerifiedEmail: true, PublicURL: "https://sync.example.test"})
	requireNoError(t, err)
	h, err := handlers.New(handlers.Options{Store: repo, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PublicURL: "http://localhost", Registration: true, AuthConcurrency: 4})
	requireNoError(t, err)
	srv := httptest.NewServer(h)
	defer srv.Close()
	client := func(token string) *remote.Client {
		c, e := remote.New(srv.URL, srv.Client(), func(context.Context) (string, error) { return token, nil })
		requireNoError(t, e)
		return c
	}
	anon := client("")
	credentials := remote.Credentials{Email: "recovery@example.test", Password: "long-original-password"}
	account, err := anon.Register(ctx, credentials)
	requireNoError(t, err)
	signed := client(account.AccessToken)
	tenants, err := signed.ListTenants(ctx, remote.Empty{})
	requireNoError(t, err)
	begin, err := anon.BeginDevice(ctx, remote.DeviceBegin{ApplicationID: "sprint", Name: "test"})
	requireNoError(t, err)
	_, err = signed.ApproveDevice(ctx, remote.DeviceApprove{UserCode: begin.UserCode, TenantID: tenants.Tenants[0].ID})
	status(t, err, 403)
	mailbox := &captureMail{fail: true}
	processed, err := maintenance.DeliverOne(ctx, pool, mailbox)
	if !processed || err == nil {
		t.Fatal("failed delivery not retried")
	}
	var attempts int
	requireNoError(t, owner.QueryRow(ctx, "SELECT attempts FROM mail_jobs").Scan(&attempts))
	if attempts != 1 {
		t.Fatal(attempts)
	}
	_, err = owner.Exec(ctx, "UPDATE mail_jobs SET available_at=now()")
	requireNoError(t, err)
	mailbox.fail = false
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := maintenance.DeliverOne(ctx, pool, mailbox); errs <- e }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		requireNoError(t, e)
	}
	if len(mailbox.messages) != 1 {
		t.Fatal("replicas delivered duplicate messages")
	}
	tokenFrom := func(m email.Message) string {
		for _, line := range strings.Split(m.Body, "\n") {
			if strings.HasPrefix(line, "https://") {
				u, e := url.Parse(line)
				requireNoError(t, e)
				return u.Query().Get("token")
			}
		}
		t.Fatal("missing recovery link")
		return ""
	}
	verify := tokenFrom(mailbox.messages[0])
	_, err = anon.ConfirmEmail(ctx, remote.ConfirmEmail{Token: verify})
	requireNoError(t, err)
	_, err = anon.ConfirmEmail(ctx, remote.ConfirmEmail{Token: verify})
	status(t, err, 401)
	_, err = signed.ApproveDevice(ctx, remote.DeviceApprove{UserCode: begin.UserCode, TenantID: tenants.Tenants[0].ID})
	requireNoError(t, err)
	grant, err := anon.PollDevice(ctx, remote.DevicePoll{DeviceCode: begin.DeviceCode})
	requireNoError(t, err)
	_, err = anon.RequestEmail(ctx, remote.RequestEmail{Email: "absent@example.test", Purpose: "reset"})
	requireNoError(t, err)
	for range 2 {
		_, err = anon.RequestEmail(ctx, remote.RequestEmail{Email: credentials.Email, Purpose: "reset"})
		requireNoError(t, err)
	}
	_, err = maintenance.DeliverOne(ctx, pool, mailbox)
	requireNoError(t, err)
	if len(mailbox.messages) != 2 {
		t.Fatal("email debounce failed")
	}
	reset := tokenFrom(mailbox.messages[1])
	_, err = anon.ConfirmEmail(ctx, remote.ConfirmEmail{Token: reset, Password: "new-long-password-123"})
	requireNoError(t, err)
	_, err = signed.ListTenants(ctx, remote.Empty{})
	status(t, err, 401)
	_, err = client(grant.AccessToken).Changes(ctx, remote.ChangesRequest{Limit: 1})
	status(t, err, 401)
	_, err = anon.RefreshDevice(ctx, remote.Refresh{RefreshToken: grant.RefreshToken})
	status(t, err, 401)
	_, err = anon.Login(ctx, credentials)
	status(t, err, 401)
	credentials.Password = "new-long-password-123"
	next, err := anon.Login(ctx, credentials)
	requireNoError(t, err)
	_, err = anon.ConfirmEmail(ctx, remote.ConfirmEmail{Token: reset, Password: "another-password-123"})
	status(t, err, 401)
	_, err = client(next.AccessToken).Logout(ctx, remote.Empty{})
	requireNoError(t, err)
	_, err = client(next.AccessToken).ListTenants(ctx, remote.Empty{})
	status(t, err, 401)
	requireNoError(t, maintenance.Run(ctx, pool))
}

func TestSharedRateLimitsIgnoreUntrustedForwarding(t *testing.T) {
	pool, _ := database(t)
	passwords, err := auth.NewPasswords(rand.Reader)
	requireNoError(t, err)
	tokens, err := auth.NewTokens(rand.Reader)
	requireNoError(t, err)
	repo, err := dbstore.New(dbstore.Options{Database: pool, Passwords: passwords, Tokens: tokens, Now: time.Now})
	requireNoError(t, err)
	makeHandler := func() http.Handler {
		h, e := handlers.New(handlers.Options{Store: repo, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PublicURL: "http://localhost", AuthConcurrency: 1, AuthRequestsPerMinute: 2})
		requireNoError(t, e)
		return h
	}
	first, second := makeHandler(), makeHandler()
	for i, h := range []http.Handler{first, second, first} {
		r := httptest.NewRequest("POST", "http://localhost/v1/auth/unknown", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		r.Header.Set("X-Real-IP", fmt.Sprintf("198.51.100.%d", i))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		expected := 404
		if i == 2 {
			expected = 429
		}
		if w.Code != expected {
			t.Fatalf("replica %d status=%d expected %d", i, w.Code, expected)
		}
	}
}
