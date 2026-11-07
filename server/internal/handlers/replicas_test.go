package handlers_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"github.com/jackc/pgx/v5/pgxpool"
	remote "github.com/tomanagle/harness-kit/packages/client-go/remote"
	"github.com/tomanagle/harness-kit/server/internal/auth"
	"github.com/tomanagle/harness-kit/server/internal/handlers"
	"github.com/tomanagle/harness-kit/server/internal/store/dbstore"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Independent pools/repositories share only Postgres. Every step switches replicas.
func TestReplicaFailover(t *testing.T) {
	pool, _ := database(t)
	ctx := context.Background()
	var replicas []*httptest.Server
	for range 3 {
		db, err := pgxpool.NewWithConfig(ctx, pool.Config().Copy())
		requireNoError(t, err)
		t.Cleanup(db.Close)
		passwords, err := auth.NewPasswords(rand.Reader)
		requireNoError(t, err)
		tokens, err := auth.NewTokens(rand.Reader)
		requireNoError(t, err)
		repository, err := dbstore.New(dbstore.Options{Database: db, Passwords: passwords, Tokens: tokens, Now: time.Now})
		requireNoError(t, err)
		handler, err := handlers.New(handlers.Options{Store: repository, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PublicURL: "http://localhost", Registration: true, AuthConcurrency: 4})
		requireNoError(t, err)
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)
		replicas = append(replicas, srv)
	}
	client := func(replica int, token string) *remote.Client {
		c, err := remote.New(replicas[replica].URL, replicas[replica].Client(), func(context.Context) (string, error) { return token, nil })
		requireNoError(t, err)
		return c
	}
	account, err := client(0, "").Register(ctx, remote.Credentials{Email: "failover@example.test", Password: "test-password-with-enough-entropy"})
	requireNoError(t, err)
	tenants, err := client(1, account.AccessToken).ListTenants(ctx, remote.Empty{})
	requireNoError(t, err)
	begin, err := client(2, "").BeginDevice(ctx, remote.DeviceBegin{ApplicationID: "replicas", Name: "device"})
	requireNoError(t, err)
	_, err = client(0, account.AccessToken).ApproveDevice(ctx, remote.DeviceApprove{UserCode: begin.UserCode, TenantID: tenants.Tenants[0].ID})
	requireNoError(t, err)
	grant, err := client(1, "").PollDevice(ctx, remote.DevicePoll{DeviceCode: begin.DeviceCode})
	requireNoError(t, err)
	batch := remote.Push{OperationID: "committed-before-failure", SessionID: "shared", Title: "Cross replica", Visibility: "private", Events: []json.RawMessage{json.RawMessage(`{"text":"exactly once"}`)}}
	ack, err := client(0, grant.AccessToken).Push(ctx, batch)
	requireNoError(t, err)
	replicas[0].Close()
	clients := []*remote.Client{client(1, grant.AccessToken), client(2, grant.AccessToken)}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, e := clients[i%2].Push(ctx, batch)
			if e == nil && got != ack {
				e = io.ErrUnexpectedEOF
			}
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		requireNoError(t, e)
	}
	events, err := client(2, grant.AccessToken).Events(ctx, remote.EventsRequest{SessionID: batch.SessionID, Limit: 100})
	requireNoError(t, err)
	if len(events.Events) != 1 || events.LastSeq != 1 {
		t.Fatalf("duplicate events after failover: %+v", events)
	}
	feed, err := client(1, grant.AccessToken).Changes(ctx, remote.ChangesRequest{Limit: 100})
	requireNoError(t, err)
	if len(feed.Changes) != 1 {
		t.Fatalf("duplicate changes: %+v", feed)
	}
	next, err := client(2, "").RefreshDevice(ctx, remote.Refresh{RefreshToken: grant.RefreshToken})
	requireNoError(t, err)
	_, err = client(1, next.AccessToken).Events(ctx, remote.EventsRequest{SessionID: batch.SessionID, Limit: 100})
	requireNoError(t, err)
	_, err = client(1, next.AccessToken).RevokeDevice(ctx, remote.Revoke{DeviceID: next.DeviceID})
	requireNoError(t, err)
	_, err = client(2, next.AccessToken).Events(ctx, remote.EventsRequest{SessionID: batch.SessionID, Limit: 100})
	status(t, err, 401)
}

func TestMetadataRevisionsAndTombstones(t *testing.T) {
	pool, owner := database(t)
	ctx := context.Background()
	passwords, err := auth.NewPasswords(rand.Reader)
	requireNoError(t, err)
	tokens, err := auth.NewTokens(rand.Reader)
	requireNoError(t, err)
	repository, err := dbstore.New(dbstore.Options{Database: pool, Passwords: passwords, Tokens: tokens, Now: time.Now})
	requireNoError(t, err)
	handler, err := handlers.New(handlers.Options{Store: repository, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PublicURL: "http://localhost", Registration: true, AuthConcurrency: 4})
	requireNoError(t, err)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	client := func(token string) *remote.Client {
		c, err := remote.New(srv.URL, srv.Client(), func(context.Context) (string, error) { return token, nil })
		requireNoError(t, err)
		return c
	}
	anon := client("")
	a, err := anon.Register(ctx, remote.Credentials{Email: "metadata@example.test", Password: "test-password-with-enough-entropy"})
	requireNoError(t, err)
	tenants, err := client(a.AccessToken).ListTenants(ctx, remote.Empty{})
	requireNoError(t, err)
	device := func() *remote.Client {
		begin, err := anon.BeginDevice(ctx, remote.DeviceBegin{ApplicationID: "sprint", Name: "device"})
		requireNoError(t, err)
		_, err = client(a.AccessToken).ApproveDevice(ctx, remote.DeviceApprove{UserCode: begin.UserCode, TenantID: tenants.Tenants[0].ID})
		requireNoError(t, err)
		grant, err := anon.PollDevice(ctx, remote.DevicePoll{DeviceCode: begin.DeviceCode})
		requireNoError(t, err)
		return client(grant.AccessToken)
	}
	writer, other := device(), device()
	_, err = writer.Push(ctx, remote.Push{OperationID: "create", SessionID: "s", Title: "Initial", Visibility: "private", Events: []json.RawMessage{json.RawMessage(`{"text":"hello"}`)}})
	requireNoError(t, err)
	update := remote.UpdateSession{OperationID: "rename", SessionID: "s", ExpectedRevision: 1, Title: "Renamed", Tags: []string{"work"}, Metadata: map[string]json.RawMessage{"sprint": json.RawMessage(`{"column":"done"}`)}}
	changed, err := other.UpdateSession(ctx, update)
	requireNoError(t, err)
	if changed.Revision != 2 || changed.Title != "Renamed" || changed.LastSeq != 1 {
		t.Fatal(changed)
	}
	retry, err := other.UpdateSession(ctx, update)
	requireNoError(t, err)
	if retry.Cursor != changed.Cursor {
		t.Fatal("duplicate revision")
	}
	stale := update
	stale.OperationID = "stale"
	_, err = writer.UpdateSession(ctx, stale)
	status(t, err, 409)
	_, err = writer.Push(ctx, remote.Push{OperationID: "next", SessionID: "s", ExpectedSeq: 1, Title: "Initial", Visibility: "private", Events: []json.RawMessage{json.RawMessage(`{"text":"after rename"}`)}})
	requireNoError(t, err)
	lease, err := writer.AcquireLease(ctx, remote.AcquireLease{SessionID: "s", ExpectedEpoch: 0})
	requireNoError(t, err)
	if lease.Epoch != 1 {
		t.Fatal(lease)
	}
	_, err = other.AcquireLease(ctx, remote.AcquireLease{SessionID: "s", ExpectedEpoch: 1})
	status(t, err, 409)
	command, err := other.SendCommand(ctx, remote.SendCommand{ID: "prompt1", SessionID: "s", Epoch: 1, Kind: "prompt", Payload: map[string]json.RawMessage{"text": json.RawMessage(`"hello"`)}})
	requireNoError(t, err)
	claimed, err := writer.ClaimCommand(ctx, remote.ClaimCommand{SessionID: "s", Epoch: 1})
	requireNoError(t, err)
	if len(claimed.Commands) != 1 || claimed.Commands[0].ID != command.ID {
		t.Fatal(claimed)
	}
	claimed, err = writer.ClaimCommand(ctx, remote.ClaimCommand{SessionID: "s", Epoch: 1})
	requireNoError(t, err)
	if len(claimed.Commands) != 0 {
		t.Fatal("claimed command redelivered")
	}
	_, err = writer.ReleaseLease(ctx, remote.RenewLease{SessionID: "s", Epoch: 1})
	status(t, err, 409)
	_, err = writer.CompleteCommand(ctx, remote.CompleteCommand{ID: command.ID, SessionID: "s", Epoch: 1, Status: "completed", Result: map[string]json.RawMessage{}})
	requireNoError(t, err)
	_, err = writer.ReleaseLease(ctx, remote.RenewLease{SessionID: "s", Epoch: 1})
	requireNoError(t, err)
	moved, err := other.AcquireLease(ctx, remote.AcquireLease{SessionID: "s", ExpectedEpoch: 1})
	requireNoError(t, err)
	if moved.Epoch != 2 {
		t.Fatal(moved)
	}
	_, err = writer.Push(ctx, remote.Push{OperationID: "stale-writer", SessionID: "s", ExpectedSeq: 2, Epoch: 1, Title: "Old", Visibility: "private"})
	status(t, err, 409)
	_, err = other.Push(ctx, remote.Push{OperationID: "new-writer", SessionID: "s", ExpectedSeq: 2, Epoch: 2, Title: "Old", Visibility: "private"})
	requireNoError(t, err)
	_, err = other.SendCommand(ctx, remote.SendCommand{ID: "uncertain", SessionID: "s", Epoch: 2, Kind: "prompt", Payload: map[string]json.RawMessage{}})
	requireNoError(t, err)
	_, err = other.ClaimCommand(ctx, remote.ClaimCommand{SessionID: "s", Epoch: 2})
	requireNoError(t, err)
	_, err = owner.Exec(ctx, "UPDATE sessions SET lease_until=now()-interval '1 second' WHERE id='s'")
	requireNoError(t, err)
	_, err = writer.AcquireLease(ctx, remote.AcquireLease{SessionID: "s", ExpectedEpoch: 2})
	requireNoError(t, err)
	uncertain, err := writer.GetCommand(ctx, remote.GetCommand{ID: "uncertain"})
	requireNoError(t, err)
	if uncertain.Status != "uncertain" {
		t.Fatal(uncertain)
	}
	deleted, err := other.DeleteSession(ctx, remote.DeleteSession{OperationID: "delete", SessionID: "s", ExpectedRevision: 2})
	requireNoError(t, err)
	if !deleted.Deleted || deleted.Revision != 3 {
		t.Fatal(deleted)
	}
	_, err = writer.Events(ctx, remote.EventsRequest{SessionID: "s", Limit: 100})
	status(t, err, 410)
	_, err = writer.Push(ctx, remote.Push{OperationID: "resurrect", SessionID: "s", ExpectedSeq: 2, Title: "Initial", Visibility: "private"})
	status(t, err, 410)
	feed, err := writer.Changes(ctx, remote.ChangesRequest{After: changed.Cursor, Limit: 100})
	requireNoError(t, err)
	if !feed.Changes[len(feed.Changes)-1].Deleted {
		t.Fatal("missing tombstone")
	}
	var count int
	requireNoError(t, owner.QueryRow(ctx, "SELECT count(*) FROM events WHERE session_id='s'").Scan(&count))
	if count != 0 {
		t.Fatal("deleted payload retained")
	}
}
