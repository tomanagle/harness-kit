package handlers_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	remote "github.com/tomanagle/harness-kit/packages/client-go/remote"

	"github.com/tomanagle/harness-kit/server/internal/auth"
	dbconnection "github.com/tomanagle/harness-kit/server/internal/database"
	"github.com/tomanagle/harness-kit/server/internal/handlers"
	"github.com/tomanagle/harness-kit/server/internal/store/dbstore"
	"github.com/tomanagle/harness-kit/server/migrations"
)

func database(t *testing.T) (*pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("HK_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set HK_TEST_DATABASE_URL to a disposable Postgres administrator database")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	requireNoError(t, err)
	tokens, err := auth.NewTokens(rand.Reader)
	requireNoError(t, err)
	id, err := tokens.ID()
	requireNoError(t, err)
	name := "hk_test_" + id
	role := name + "_app"
	_, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	requireNoError(t, err)
	_, err = admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" NOSUPERUSER NOBYPASSRLS NOLOGIN")
	requireNoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		_, _ = admin.Exec(ctx, "DROP ROLE "+pgx.Identifier{role}.Sanitize())
		admin.Close()
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	requireNoError(t, err)
	cfg.ConnConfig.Database = name
	owner, err := pgxpool.NewWithConfig(ctx, cfg)
	requireNoError(t, err)
	t.Cleanup(owner.Close)
	requireNoError(t, migrations.Apply(ctx, owner))
	requireNoError(t, migrations.Apply(ctx, owner))
	_, err = owner.Exec(ctx, "GRANT USAGE ON SCHEMA public TO "+pgx.Identifier{role}.Sanitize())
	requireNoError(t, err)
	_, err = owner.Exec(ctx, "GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA public TO "+pgx.Identifier{role}.Sanitize())
	requireNoError(t, err)
	appCfg := cfg.Copy()
	appCfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
		return err
	}
	app, err := pgxpool.NewWithConfig(ctx, appCfg)
	requireNoError(t, err)
	t.Cleanup(app.Close)
	requireNoError(t, dbconnection.CheckRuntimeRole(ctx, app))
	if dbconnection.CheckRuntimeRole(ctx, owner) == nil {
		t.Fatal("schema owner accepted as runtime role")
	}
	return app, owner
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func status(t *testing.T, err error, want int) {
	t.Helper()
	var e *remote.HTTPError
	if !errors.As(err, &e) || e.Status != want {
		t.Fatalf("want HTTP %d, got %v", want, err)
	}
}

func TestPostgresDeviceSync(t *testing.T) {
	pool, owner := database(t)
	ctx := context.Background()
	passwords, err := auth.NewPasswords(rand.Reader)
	requireNoError(t, err)
	tokens, err := auth.NewTokens(rand.Reader)
	requireNoError(t, err)
	s, err := dbstore.New(dbstore.Options{Database: pool, Passwords: passwords, Tokens: tokens, Now: time.Now})
	requireNoError(t, err)
	srv := httptest.NewUnstartedServer(nil)
	origin := "http://" + srv.Listener.Addr().String()
	srv.Config.Handler, err = handlers.New(handlers.Options{Store: s, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PublicURL: origin, Registration: true, AuthConcurrency: 4})
	requireNoError(t, err)
	srv.Start()
	defer srv.Close()
	client := func(token string) *remote.Client {
		c, err := remote.New(srv.URL, srv.Client(), func(context.Context) (string, error) { return token, nil })
		requireNoError(t, err)
		return c
	}
	anon := client("")
	account := func(email string) (remote.Account, *remote.Client) {
		a, err := anon.Register(ctx, remote.Credentials{Email: email, Password: "test-password-with-enough-entropy"})
		requireNoError(t, err)
		return a, client(a.AccessToken)
	}
	alice, a := account("alice@example.test")
	bobAccount, b := account("bob@example.test")
	_, v := account("viewer@example.test")
	_, outside := account("outside@example.test")
	personal, err := a.ListTenants(ctx, remote.Empty{})
	requireNoError(t, err)
	if len(personal.Tenants) != 1 {
		t.Fatal(personal)
	}
	work, err := a.CreateTenant(ctx, remote.CreateTenant{Name: "Work"})
	requireNoError(t, err)
	_, err = a.AddMember(ctx, remote.AddMember{TenantID: work.ID, Email: "bob@example.test", Role: "member"})
	requireNoError(t, err)
	_, err = a.AddMember(ctx, remote.AddMember{TenantID: work.ID, Email: "viewer@example.test", Role: "viewer"})
	requireNoError(t, err)
	_, err = b.AddMember(ctx, remote.AddMember{TenantID: work.ID, Email: "outside@example.test", Role: "admin"})
	status(t, err, 403)
	tenants, err := a.ListTenants(ctx, remote.Empty{})
	requireNoError(t, err)
	if len(tenants.Tenants) != 2 {
		t.Fatal(tenants)
	}
	device := func(ac *remote.Client, tenant, app string) (remote.Grant, *remote.Client) {
		begin, err := anon.BeginDevice(ctx, remote.DeviceBegin{ApplicationID: app, Name: "Test laptop"})
		requireNoError(t, err)
		if begin.VerificationURI != origin+"/device" {
			t.Fatal(begin)
		}
		_, err = ac.ApproveDevice(ctx, remote.DeviceApprove{UserCode: begin.UserCode, TenantID: tenant})
		requireNoError(t, err)
		grant, err := anon.PollDevice(ctx, remote.DevicePoll{DeviceCode: begin.DeviceCode})
		requireNoError(t, err)
		_, err = anon.PollDevice(ctx, remote.DevicePoll{DeviceCode: begin.DeviceCode})
		status(t, err, 400)
		return grant, client(grant.AccessToken)
	}
	workGrant, writer := device(a, work.ID, "sprint")
	_, reader := device(a, work.ID, "sprint")
	_, personalClient := device(a, personal.Tenants[0].ID, "sprint")
	_, otherApp := device(a, work.ID, "other-app")
	_, bob := device(b, work.ID, "sprint")
	_, viewer := device(v, work.ID, "sprint")
	begin, err := anon.BeginDevice(ctx, remote.DeviceBegin{ApplicationID: "sprint", Name: "pending"})
	requireNoError(t, err)
	_, err = outside.ApproveDevice(ctx, remote.DeviceApprove{UserCode: begin.UserCode, TenantID: work.ID})
	status(t, err, 403)
	_, err = anon.PollDevice(ctx, remote.DevicePoll{DeviceCode: begin.DeviceCode})
	status(t, err, 400)
	_, err = anon.PollDevice(ctx, remote.DevicePoll{DeviceCode: begin.DeviceCode})
	status(t, err, 429)
	batch := remote.Push{OperationID: "op-1", SessionID: "session-1", Title: "Private work", Visibility: "private", Events: []json.RawMessage{json.RawMessage(`{"type":"message","text":"hello"}`)}}
	ack, err := writer.Push(ctx, batch)
	requireNoError(t, err)
	if ack.LastSeq != 1 || ack.Cursor != 1 {
		t.Fatal(ack)
	}
	// An acknowledged batch can be retried concurrently without another event/cursor.
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, e := writer.Push(ctx, batch)
			if e == nil && got != ack {
				e = fmt.Errorf("different acknowledgement: %+v", got)
			}
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		requireNoError(t, e)
	}
	changed := batch
	changed.Title = "different"
	_, err = writer.Push(ctx, changed)
	status(t, err, 409)
	page, err := reader.Events(ctx, remote.EventsRequest{SessionID: batch.SessionID, Limit: 100})
	requireNoError(t, err)
	if len(page.Events) != 1 || page.Events[0].Seq != 1 {
		t.Fatal(page)
	}
	_, err = reader.Push(ctx, batch)
	status(t, err, 403)
	for _, c := range []*remote.Client{personalClient, otherApp, bob, viewer} {
		_, err = c.Events(ctx, remote.EventsRequest{SessionID: batch.SessionID})
		status(t, err, 404)
	}
	shared := remote.Push{OperationID: "shared-op", SessionID: "shared", Title: "Team", Visibility: "shared", Events: batch.Events}
	shared.Events = append(shared.Events, json.RawMessage(`{"type":"message","text":"second"}`))
	_, err = writer.Push(ctx, shared)
	requireNoError(t, err)
	firstPage, err := viewer.Events(ctx, remote.EventsRequest{SessionID: "shared", Limit: 1})
	requireNoError(t, err)
	if firstPage.LastSeq != 2 || firstPage.NextAfter != 1 || !firstPage.HasMore {
		t.Fatalf("invalid first page: %+v", firstPage)
	}
	secondPage, err := viewer.Events(ctx, remote.EventsRequest{SessionID: "shared", After: firstPage.NextAfter, Limit: 1})
	requireNoError(t, err)
	if secondPage.NextAfter != 2 || secondPage.HasMore || len(secondPage.Events) != 1 {
		t.Fatalf("invalid second page: %+v", secondPage)
	}
	_, err = viewer.Push(ctx, remote.Push{OperationID: "viewer-op", SessionID: "viewer", Visibility: "shared"})
	status(t, err, 403)
	visible, err := bob.Changes(ctx, remote.ChangesRequest{Limit: 100})
	requireNoError(t, err)
	if len(visible.Changes) != 1 || visible.Cursor != 2 {
		t.Fatal(visible)
	}
	empty, err := otherApp.Changes(ctx, remote.ChangesRequest{})
	requireNoError(t, err)
	if len(empty.Changes) != 0 || empty.Cursor != 0 {
		t.Fatal(empty)
	}
	// Postgres itself denies cross-scope access even when a query omits WHERE.
	var count int
	requireNoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM events").Scan(&count))
	if count != 0 {
		t.Fatal("RLS leaked events without scope")
	}
	tx, err := pool.Begin(ctx)
	requireNoError(t, err)
	_, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true),set_config('app.application_id','other-app',true)", work.ID)
	requireNoError(t, err)
	requireNoError(t, tx.QueryRow(ctx, "SELECT count(*) FROM events").Scan(&count))
	if count != 0 {
		t.Fatal("RLS leaked another app")
	}
	requireNoError(t, tx.Rollback(ctx))
	// Parallel transactions get a durable contiguous order, replayed in small pages.
	errs = make(chan error, 12)
	for i := range 12 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, e := writer.Push(ctx, remote.Push{OperationID: fmt.Sprintf("parallel-%d", i), SessionID: fmt.Sprintf("parallel-%d", i), Visibility: "private"})
			errs <- e
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		requireNoError(t, e)
	}
	cursor := int64(0)
	total := 0
	for {
		changes, err := reader.Changes(ctx, remote.ChangesRequest{After: cursor, Limit: 3})
		requireNoError(t, err)
		for _, c := range changes.Changes {
			if c.Cursor != int64(total+1) {
				t.Fatalf("gap: %+v", changes)
			}
			total++
		}
		cursor = changes.Cursor
		if len(changes.Changes) == 0 {
			break
		}
	}
	if total != 14 {
		t.Fatalf("changes: %d", total)
	}
	// Rotation invalidates reuse, including the newly issued access token.
	rotating, rotClient := device(a, work.ID, "rotate-test")
	next, err := anon.RefreshDevice(ctx, remote.Refresh{RefreshToken: rotating.RefreshToken})
	requireNoError(t, err)
	_, err = anon.RefreshDevice(ctx, remote.Refresh{RefreshToken: rotating.RefreshToken})
	status(t, err, 401)
	_, err = client(next.AccessToken).Changes(ctx, remote.ChangesRequest{})
	status(t, err, 401)
	_, err = rotClient.Changes(ctx, remote.ChangesRequest{})
	status(t, err, 401)
	// Revoking this Work grant cannot log out Personal or the other Work device.
	_, err = a.RevokeDevice(ctx, remote.Revoke{DeviceID: workGrant.DeviceID})
	requireNoError(t, err)
	_, err = writer.Changes(ctx, remote.ChangesRequest{})
	status(t, err, 401)
	_, err = personalClient.Push(ctx, batch)
	requireNoError(t, err)
	_, err = reader.Events(ctx, remote.EventsRequest{SessionID: batch.SessionID})
	requireNoError(t, err)
	// Role edits and inventories are account-scoped; existing grants observe changes.
	members, err := a.ListMembers(ctx, remote.TenantRequest{TenantID: work.ID})
	requireNoError(t, err)
	if len(members.Members) != 3 {
		t.Fatal(members)
	}
	_, err = b.ListMembers(ctx, remote.TenantRequest{TenantID: work.ID})
	status(t, err, 403)
	_, err = a.SetMemberRole(ctx, remote.SetMemberRole{TenantID: work.ID, AccountID: bobAccount.AccountID, Role: "admin"})
	requireNoError(t, err)
	_, err = b.SetMemberRole(ctx, remote.SetMemberRole{TenantID: work.ID, AccountID: alice.AccountID, Role: "member"})
	status(t, err, 403)
	_, err = b.SetMemberRole(ctx, remote.SetMemberRole{TenantID: work.ID, AccountID: bobAccount.AccountID, Role: "member"})
	status(t, err, 403)
	_, err = a.SetMemberRole(ctx, remote.SetMemberRole{TenantID: work.ID, AccountID: bobAccount.AccountID, Role: "viewer"})
	requireNoError(t, err)
	_, err = bob.Push(ctx, remote.Push{OperationID: "downgraded", SessionID: "downgraded", Visibility: "shared", Title: "test"})
	status(t, err, 403)
	devices, err := a.ListDevices(ctx, remote.Empty{})
	requireNoError(t, err)
	found := false
	for _, device := range devices.Devices {
		if device.ID == workGrant.DeviceID {
			found = device.Revoked
		}
	}
	if !found {
		t.Fatal("revoked device missing from inventory")
	}
	// Administrators revoke membership through the API without access to private content.
	_, err = a.RemoveMember(ctx, remote.RemoveMember{TenantID: work.ID, AccountID: alice.AccountID})
	status(t, err, 403) // ownership transfer must be explicit
	_, err = a.RemoveMember(ctx, remote.RemoveMember{TenantID: work.ID, AccountID: bobAccount.AccountID})
	requireNoError(t, err)
	_, err = bob.Changes(ctx, remote.ChangesRequest{})
	status(t, err, 401)
	// Membership removal takes effect for already issued tokens.
	_, err = owner.Exec(ctx, "DELETE FROM memberships WHERE tenant_id=$1 AND user_id=$2", work.ID, alice.AccountID)
	requireNoError(t, err)
	_, err = reader.Changes(ctx, remote.ChangesRequest{})
	status(t, err, 401)
	_, err = personalClient.Changes(ctx, remote.ChangesRequest{})
	requireNoError(t, err)
	// Cookie form mutations reject cross-origin requests before authentication.
	req, err := http.NewRequest(http.MethodPost, origin+"/device", strings.NewReader("code=bad&tenant=bad"))
	requireNoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://attacker.test")
	res, err := srv.Client().Do(req)
	requireNoError(t, err)
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal(res.StatusCode)
	}
	// Cancellation terminates long polls rather than keeping pool connections busy.
	cancelCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = personalClient.Changes(cancelCtx, remote.ChangesRequest{After: 1, WaitMs: 25000})
	if err == nil {
		t.Fatal("expected cancellation")
	}
}

// A token generator failure after rows have been inserted must roll back the
// whole registration. Dependency injection exercises the real transaction path.
type failedTokens struct {
	auth.Tokens
	failure error
}

func (f failedTokens) Token() (string, error) { return "", f.failure }

func TestRegistrationRollsBackOnTokenFailure(t *testing.T) {
	pool, owner := database(t)
	tokens, err := auth.NewTokens(rand.Reader)
	requireNoError(t, err)
	passwords, err := auth.NewPasswords(rand.Reader)
	requireNoError(t, err)
	failure := errors.New("token source unavailable")
	repository, err := dbstore.New(dbstore.Options{Database: pool, Passwords: passwords, Tokens: failedTokens{Tokens: tokens, failure: failure}, Now: time.Now})
	requireNoError(t, err)
	_, err = repository.Register(context.Background(), "rollback@example.test", "long-enough-password")
	if !errors.Is(err, failure) {
		t.Fatalf("unexpected failure: %v", err)
	}
	var users, tenants, memberships int
	requireNoError(t, owner.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM users),(SELECT count(*) FROM tenants),(SELECT count(*) FROM memberships)").Scan(&users, &tenants, &memberships))
	if users != 0 || tenants != 0 || memberships != 0 {
		t.Fatalf("registration partially committed: users=%d tenants=%d memberships=%d", users, tenants, memberships)
	}
}
