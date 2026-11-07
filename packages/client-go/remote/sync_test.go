package remote

import (
	"context"
	"encoding/json"
	"errors"
	kit "github.com/tomanagle/harness-kit/packages/client-go"
	"reflect"
	"testing"
)

type source struct{}

func (source) SessionDurableEventsPage(_ context.Context, p kit.SessionEventsParams) (kit.EventPage, error) {
	events := []kit.HarnessEvent{}
	for n := p.AfterSeq + 1; n <= 7 && len(events) < p.Limit; n++ {
		events = append(events, kit.HarnessEvent{SessionID: p.SessionID, Seq: n})
	}
	return kit.EventPage{Events: events}, nil
}

type syncAPI struct {
	*Client
	events   []Event
	changes  []Change
	receipts map[string]Ack
	calls    []Push
	lost     bool
}

func (a *syncAPI) Push(_ context.Context, p Push) (Ack, error) {
	a.calls = append(a.calls, p)
	if ack, ok := a.receipts[p.OperationID]; ok {
		return ack, nil
	}
	if p.ExpectedSeq != int64(len(a.events)) {
		return Ack{}, errors.New("conflict")
	}
	for i, event := range p.Events {
		a.events = append(a.events, Event{Seq: p.ExpectedSeq + int64(i) + 1, Data: event})
	}
	change := Change{Cursor: int64(len(a.changes) + 1), SessionID: p.SessionID, LastSeq: int64(len(a.events)), Title: p.Title, Revision: 1}
	a.changes = append(a.changes, change)
	ack := Ack{SessionID: p.SessionID, LastSeq: change.LastSeq, Cursor: change.Cursor}
	a.receipts[p.OperationID] = ack
	if a.lost {
		a.lost = false
		return Ack{}, errors.New("lost ACK")
	}
	return ack, nil
}
func (a *syncAPI) Changes(_ context.Context, p ChangesRequest) (Changes, error) {
	for _, c := range a.changes {
		if c.Cursor > p.After {
			return Changes{Changes: []Change{c}, Cursor: c.Cursor}, nil
		}
	}
	return Changes{Cursor: p.After}, nil
}
func (a *syncAPI) Events(_ context.Context, p EventsRequest) (Events, error) {
	end := min(len(a.events), int(p.After)+p.Limit)
	return Events{Events: a.events[p.After:end], NextAfter: int64(end), LastSeq: int64(len(a.events))}, nil
}
func TestSyncRestartAfterLostAcknowledgement(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewFileSyncStore(dir, FileSyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	api := &syncAPI{receipts: map[string]Ack{}, lost: true}
	scope := Scope{ServerID: "work", URL: "https://example.test", AccountID: "alice", TenantID: "work", ApplicationID: "sprint"}
	options := SyncOptions{Scope: scope, Store: store, Source: source{}, Protocol: api, Settings: func(context.Context) (kit.SyncSettings, error) {
		return kit.SyncSettings{BatchEvents: 2, WorkPerCycle: 2, PollIntervalMs: 100}, nil
	}}
	worker, err := NewSync(options)
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.Publish(ctx, "session", "Title", "private"); err != nil {
		t.Fatal(err)
	}
	if err = worker.Once(ctx); err == nil {
		t.Fatal("lost response not reported")
	}
	state, err := store.Load(ctx, scope.Key())
	if err != nil || state.Bindings[0].Pending == nil {
		t.Fatal("pending operation missing", err)
	}
	reopened, err := NewFileSyncStore(dir, FileSyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	options.Store = reopened
	worker, err = NewSync(options)
	if err != nil {
		t.Fatal(err)
	}
	for range 8 {
		if err = worker.Once(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(api.calls[0], api.calls[1]) || len(api.events) != 7 {
		t.Fatal("retry was not identical")
	}
	events, err := worker.Events(ctx, "session", 0, 100)
	if err != nil || len(events) != 7 {
		t.Fatal(events, err)
	}
	options.Scope.TenantID = "personal"
	other, err := NewSync(options)
	if err != nil {
		t.Fatal(err)
	}
	if err = other.Publish(ctx, "session", "Title", "private"); err == nil {
		t.Fatal("destination changed")
	}
	deleted := api.changes[len(api.changes)-1]
	deleted.Cursor++
	deleted.Deleted = true
	api.changes = append(api.changes, deleted)
	if err = worker.Once(ctx); err != nil {
		t.Fatal(err)
	}
	events, err = worker.Events(ctx, "session", 0, 100)
	if err != nil || len(events) != 0 {
		t.Fatal("tombstone did not hide history")
	}
}
func TestFileCacheBoundsAndChunkRecovery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := NewFileSyncStore(dir, FileSyncOptions{MaxSessions: 1})
	if err != nil {
		t.Fatal(err)
	}
	c := Change{SessionID: "s", Cursor: 1, LastSeq: 73}
	events := []Event{}
	for n := int64(1); n <= 73; n++ {
		events = append(events, Event{Seq: n, Data: json.RawMessage(`{"text":"hello"}`)})
	}
	if err = s.Append(ctx, "scope", c, events[:23]); err != nil {
		t.Fatal(err)
	}
	if err = s.Append(ctx, "scope", c, events[23:]); err != nil {
		t.Fatal(err)
	}
	got, err := s.Events(ctx, "scope", "s", 24, 40)
	if err != nil || !reflect.DeepEqual(got, events[24:64]) {
		t.Fatal("chunk paging failed", err)
	}
	if err = s.Append(ctx, "scope", Change{SessionID: "another", Deleted: true}, nil); err == nil {
		t.Fatal("tombstones bypassed capacity")
	}
}

type leaseAPI struct {
	*syncAPI
	committed bool
	epoch     int64
}

func (a *leaseAPI) Push(ctx context.Context, p Push) (Ack, error) {
	if a.committed {
		return a.syncAPI.Push(ctx, p)
	}
	return Ack{}, &HTTPError{Status: 409, Code: "conflict"}
}
func (a *leaseAPI) GetSession(context.Context, GetSession) (Change, error) {
	return Change{SessionID: "session", WriterID: "device", WriterEpoch: a.epoch, LastSeq: 0}, nil
}
func TestUseLeaseReconcilesPendingUploads(t *testing.T) {
	for _, committed := range []bool{false, true} {
		ctx := context.Background()
		store, err := NewFileSyncStore(t.TempDir(), FileSyncOptions{})
		if err != nil {
			t.Fatal(err)
		}
		underlying := &syncAPI{receipts: map[string]Ack{}, lost: true}
		api := &leaseAPI{syncAPI: underlying, committed: committed, epoch: 2}
		scope := Scope{ServerID: "work", URL: "https://example.test", AccountID: "alice", TenantID: "work", ApplicationID: "sprint"}
		worker, err := NewSync(SyncOptions{Scope: scope, Store: store, Source: source{}, Protocol: api, Settings: func(context.Context) (kit.SyncSettings, error) {
			return kit.SyncSettings{BatchEvents: 2, WorkPerCycle: 2}, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err = worker.Publish(ctx, "session", "Title", "private"); err != nil {
			t.Fatal(err)
		}
		if err = worker.Once(ctx); err == nil {
			t.Fatal("expected lost ACK/conflict")
		}
		before, err := store.Load(ctx, scope.Key())
		if err != nil {
			t.Fatal(err)
		}
		if err = worker.UseLease(ctx, Lease{SessionID: "session", DeviceID: "device", Epoch: 2}); err != nil {
			t.Fatal(err)
		}
		after, err := store.Load(ctx, scope.Key())
		if err != nil {
			t.Fatal(err)
		}
		b := after.Bindings[0]
		if b.Epoch != 2 {
			t.Fatal(b)
		}
		if committed {
			if b.Pending != nil || b.UploadedSeq != 2 {
				t.Fatal(b)
			}
		} else if b.Pending == nil || b.Pending.OperationID == before.Bindings[0].Pending.OperationID || b.Pending.Epoch != 2 {
			t.Fatal("pending not safely rebased")
		}
	}
}
