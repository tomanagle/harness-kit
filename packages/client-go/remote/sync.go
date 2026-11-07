package remote

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	kit "github.com/tomanagle/harness-kit/packages/client-go"
)

// History must return durable, contiguous events. Never substitute a UI buffer.
type History interface {
	SessionDurableEventsPage(context.Context, kit.SessionEventsParams) (kit.EventPage, error)
}
type SyncProtocol interface {
	Push(context.Context, Push) (Ack, error)
	Changes(context.Context, ChangesRequest) (Changes, error)
	Events(context.Context, EventsRequest) (Events, error)
	UpdateSession(context.Context, UpdateSession) (Change, error)
	DeleteSession(context.Context, DeleteSession) (Change, error)
	SendCommand(context.Context, SendCommand) (Command, error)
	GetSession(context.Context, GetSession) (Change, error)
}
type Binding struct {
	SessionID   string
	Title       string
	Visibility  string
	UploadedSeq int64
	Epoch       int64
	Deleted     bool
	Pending     *Push
}
type Mutation struct {
	Update *UpdateSession
	Delete *DeleteSession
}
type Checkpoint struct {
	Cursor    int64
	Bindings  []Binding
	Mutations []Mutation
	Commands  []SendCommand
}
type CachedSession struct {
	Change
	CachedSeq int64
}

// SyncStore has one writer. Save and Append must be atomic and durable before
// returning; Append publishes a head only after its contiguous events are durable.
type SyncStore interface {
	Claim(context.Context, string, string) error
	Load(context.Context, string) (Checkpoint, error)
	Save(context.Context, string, Checkpoint) error
	List(context.Context, string) ([]CachedSession, error)
	Append(context.Context, string, Change, []Event) error
	Events(context.Context, string, string, int64, int) ([]Event, error)
}
type SyncOptions struct {
	Scope    Scope
	Protocol SyncProtocol
	Store    SyncStore
	Source   History
	Settings func(context.Context) (kit.SyncSettings, error)
}
type Sync struct {
	options  SyncOptions
	key      string
	mu       sync.Mutex
	offset   int
	statusMu sync.RWMutex
	status   SyncStatus
}
type SyncStatus struct {
	Phase string
	Code  string
}

func NewSync(o SyncOptions) (*Sync, error) {
	if o.Protocol == nil || o.Store == nil || o.Source == nil || o.Settings == nil || o.Scope.AccountID == "" || o.Scope.TenantID == "" || o.Scope.ApplicationID == "" {
		return nil, errors.New("remote: sync dependencies and scope required")
	}
	return &Sync{options: o, key: o.Scope.Key(), status: SyncStatus{Phase: "stopped"}}, nil
}
func (s *Sync) Status() SyncStatus { s.statusMu.RLock(); defer s.statusMu.RUnlock(); return s.status }
func (s *Sync) set(status SyncStatus) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.status = status
}
func operationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func (s *Sync) Publish(ctx context.Context, id, title, visibility string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validID(id) || len(title) > 500 || (visibility != "private" && visibility != "shared") {
		return errors.New("remote: invalid binding")
	}
	state, err := s.options.Store.Load(ctx, s.key)
	if err != nil {
		return err
	}
	for _, b := range state.Bindings {
		if b.SessionID == id {
			if b.Title != title || b.Visibility != visibility {
				return errors.New("remote: immutable binding")
			}
			return nil
		}
	}
	page, err := s.options.Source.SessionDurableEventsPage(ctx, kit.SessionEventsParams{SessionID: id, Limit: 1})
	if err != nil {
		return err
	}
	if page.Truncated {
		return errors.New("remote: history gap")
	}
	if err = s.options.Store.Claim(ctx, id, s.key); err != nil {
		return err
	}
	state.Bindings = append(state.Bindings, Binding{SessionID: id, Title: title, Visibility: visibility})
	return s.options.Store.Save(ctx, s.key, state)
}
func (s *Sync) Sessions(ctx context.Context) ([]CachedSession, error) {
	return s.options.Store.List(ctx, s.key)
}
func (s *Sync) Events(ctx context.Context, id string, after int64, limit int) ([]Event, error) {
	return s.options.Store.Events(ctx, s.key, id, after, limit)
}
func (s *Sync) UseLease(ctx context.Context, l Lease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.options.Store.Load(ctx, s.key)
	if err != nil {
		return err
	}
	for i := range state.Bindings {
		b := &state.Bindings[i]
		if b.SessionID == l.SessionID {
			if l.Epoch < 1 {
				return errors.New("remote: invalid lease")
			}
			if b.Pending != nil {
				batch := *b.Pending
				ack, e := s.options.Protocol.Push(ctx, batch)
				if e == nil {
					if ack.SessionID != batch.SessionID || ack.LastSeq != batch.ExpectedSeq+int64(len(batch.Events)) {
						return errors.New("remote: invalid acknowledgement")
					}
					b.UploadedSeq = ack.LastSeq
					b.Pending = nil
				} else {
					var response *HTTPError
					if !errors.As(e, &response) || response.Status != 409 {
						return e
					}
					current, e := s.options.Protocol.GetSession(ctx, GetSession{SessionID: l.SessionID})
					if e != nil {
						return e
					}
					if current.WriterID != l.DeviceID || current.WriterEpoch != l.Epoch || current.LastSeq != batch.ExpectedSeq || current.Deleted {
						return errors.New("remote: history conflict")
					}
					id, e := operationID()
					if e != nil {
						return e
					}
					batch.OperationID = id
					batch.Epoch = l.Epoch
					b.Pending = &batch
				}
			}
			b.Epoch = l.Epoch
			return s.options.Store.Save(ctx, s.key, state)
		}
	}
	return errors.New("remote: binding missing")
}

// Queue mutations/commands before network I/O. Keep returned IDs to reconcile an
// uncertain outcome. Conflict resolution is explicit, never last-writer-wins.
func (s *Sync) Update(ctx context.Context, in UpdateSession) (string, error) {
	id, err := operationID()
	if err != nil {
		return "", err
	}
	in.OperationID = id
	return id, s.queue(ctx, func(c *Checkpoint) { c.Mutations = append(c.Mutations, Mutation{Update: &in}) })
}
func (s *Sync) Delete(ctx context.Context, in DeleteSession) (string, error) {
	id, err := operationID()
	if err != nil {
		return "", err
	}
	in.OperationID = id
	return id, s.queue(ctx, func(c *Checkpoint) { c.Mutations = append(c.Mutations, Mutation{Delete: &in}) })
}
func (s *Sync) Command(ctx context.Context, in SendCommand) (string, error) {
	id, err := operationID()
	if err != nil {
		return "", err
	}
	in.ID = id
	return id, s.queue(ctx, func(c *Checkpoint) { c.Commands = append(c.Commands, in) })
}
func (s *Sync) DiscardMutation(ctx context.Context, id string) error {
	return s.queue(ctx, func(c *Checkpoint) {
		out := c.Mutations[:0]
		for _, m := range c.Mutations {
			if m.Update != nil && m.Update.OperationID == id || m.Delete != nil && m.Delete.OperationID == id {
				continue
			}
			out = append(out, m)
		}
		c.Mutations = out
	})
}
func (s *Sync) queue(ctx context.Context, edit func(*Checkpoint)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.options.Store.Load(ctx, s.key)
	if err != nil {
		return err
	}
	edit(&c)
	return s.options.Store.Save(ctx, s.key, c)
}
func (s *Sync) Once(ctx context.Context) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.set(SyncStatus{Phase: "syncing"})
	defer func() {
		if err != nil {
			status := SyncStatus{Phase: "blocked", Code: "local_sync_error"}
			var e *HTTPError
			if errors.As(err, &e) {
				status.Code = e.Code
				if e.Status == 401 || e.Status == 403 {
					status.Phase = "authentication_required"
				} else if e.Status >= 500 || e.Status == 429 {
					status.Phase = "offline"
				}
			}
			s.set(status)
		}
	}()
	p, err := s.options.Settings(ctx)
	if err != nil {
		return err
	}
	if p.Paused {
		s.set(SyncStatus{Phase: "paused"})
		return nil
	}
	if p.BatchEvents < 1 || p.BatchEvents > 100 || p.WorkPerCycle < 1 || p.WorkPerCycle > 100 {
		return errors.New("remote: invalid sync policy")
	}
	store, api := s.options.Store, s.options.Protocol
	state, err := store.Load(ctx, s.key)
	if err != nil {
		return err
	}
	var failures []error
	for n := 0; n < p.WorkPerCycle && len(state.Mutations) > 0; n++ {
		m := state.Mutations[0]
		var got Change
		var id string
		var revision int64
		if m.Update != nil {
			id = m.Update.SessionID
			revision = m.Update.ExpectedRevision
			got, err = api.UpdateSession(ctx, *m.Update)
		} else if m.Delete != nil {
			id = m.Delete.SessionID
			revision = m.Delete.ExpectedRevision
			got, err = api.DeleteSession(ctx, *m.Delete)
		} else {
			return errors.New("remote: invalid mutation")
		}
		if err == nil && (got.SessionID != id || got.Revision != revision+1) {
			err = errors.New("remote: invalid acknowledgement")
		}
		if err != nil {
			failures = append(failures, err)
			break
		}
		state.Mutations = state.Mutations[1:]
		if err = store.Save(ctx, s.key, state); err != nil {
			return err
		}
	}
	for n := 0; n < p.WorkPerCycle && len(state.Commands) > 0; n++ {
		command := state.Commands[0]
		ack, e := api.SendCommand(ctx, command)
		if e == nil && (ack.ID != command.ID || ack.SessionID != command.SessionID || ack.Epoch != command.Epoch) {
			e = errors.New("remote: invalid command acknowledgement")
		}
		if e != nil {
			failures = append(failures, e)
			break
		}
		state.Commands = state.Commands[1:]
		if err = store.Save(ctx, s.key, state); err != nil {
			return err
		}
	}
	for n := 0; n < min(p.WorkPerCycle, len(state.Bindings)); n++ {
		index := s.offset % len(state.Bindings)
		s.offset++
		b := &state.Bindings[index]
		if b.Deleted {
			continue
		}
		e := func() error {
			if b.Pending == nil {
				page, e := s.options.Source.SessionDurableEventsPage(ctx, kit.SessionEventsParams{SessionID: b.SessionID, AfterSeq: uint64(b.UploadedSeq), Limit: p.BatchEvents})
				if e != nil {
					return e
				}
				if page.Truncated {
					return errors.New("remote: history gap")
				}
				events := []json.RawMessage{}
				bytes := 0
				seq := b.UploadedSeq
				for _, event := range page.Events {
					if event.SessionID != b.SessionID || event.Seq != uint64(seq+1) {
						return errors.New("remote: history gap")
					}
					data, e := json.Marshal(event)
					if e != nil {
						return e
					}
					if len(data) > 65536 {
						return errors.New("remote: event too large")
					}
					if bytes+len(data) > 900000 {
						break
					}
					events = append(events, data)
					bytes += len(data)
					seq++
				}
				if len(events) == 0 {
					return nil
				}
				id, e := operationID()
				if e != nil {
					return e
				}
				b.Pending = &Push{OperationID: id, SessionID: b.SessionID, ExpectedSeq: b.UploadedSeq, Epoch: b.Epoch, Title: b.Title, Visibility: b.Visibility, Events: events}
				if e = store.Save(ctx, s.key, state); e != nil {
					return e
				}
			}
			ack, e := api.Push(ctx, *b.Pending)
			if e != nil {
				return e
			}
			if ack.SessionID != b.SessionID || ack.LastSeq != b.Pending.ExpectedSeq+int64(len(b.Pending.Events)) {
				return errors.New("remote: invalid acknowledgement")
			}
			b.UploadedSeq = ack.LastSeq
			b.Pending = nil
			return store.Save(ctx, s.key, state)
		}()
		if e != nil {
			failures = append(failures, e)
			state, err = store.Load(ctx, s.key)
			if err != nil {
				return err
			}
		}
	}
	for n := 0; n < p.WorkPerCycle; n++ {
		feed, e := api.Changes(ctx, ChangesRequest{After: state.Cursor, Limit: 1})
		if e != nil {
			return e
		}
		if feed.Cursor < state.Cursor || len(feed.Changes) > 1 {
			return errors.New("remote: invalid cursor")
		}
		if len(feed.Changes) > 0 {
			change := feed.Changes[0]
			if change.Cursor <= state.Cursor || change.Cursor > feed.Cursor || change.LastSeq < 0 {
				return errors.New("remote: invalid change")
			}
			cached, e := store.List(ctx, s.key)
			if e != nil {
				return e
			}
			var head int64
			for _, c := range cached {
				if c.SessionID == change.SessionID {
					head = c.CachedSeq
				}
			}
			var events []Event
			if !change.Deleted && head < change.LastSeq {
				page, e := api.Events(ctx, EventsRequest{SessionID: change.SessionID, After: head, Limit: p.BatchEvents})
				if e != nil {
					return e
				}
				if len(page.Events) == 0 || len(page.Events) > p.BatchEvents {
					return errors.New("remote: history gap")
				}
				for _, event := range page.Events {
					head++
					if event.Seq != head {
						return errors.New("remote: history gap")
					}
				}
				if page.NextAfter != head {
					return errors.New("remote: history gap")
				}
				events = page.Events
			}
			if e = store.Append(ctx, s.key, change, events); e != nil {
				return e
			}
			if change.Deleted {
				for i := range state.Bindings {
					if state.Bindings[i].SessionID == change.SessionID {
						state.Bindings[i].Deleted = true
						state.Bindings[i].Pending = nil
					}
				}
			} else if head < change.LastSeq {
				continue
			}
		}
		state.Cursor = feed.Cursor
		if err = store.Save(ctx, s.key, state); err != nil {
			return err
		}
		if len(feed.Changes) == 0 {
			break
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	s.set(SyncStatus{Phase: "idle"})
	return nil
}

// Run observes policy changes each cycle. Cancel ctx to stop. Transient failures
// back off; callers can inspect Status without stopping the worker.
func (s *Sync) Run(ctx context.Context) error {
	defer s.set(SyncStatus{Phase: "stopped"})
	failures := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.Once(ctx); err != nil {
			failures++
		} else {
			failures = 0
		}
		interval := time.Second
		if p, e := s.options.Settings(ctx); e == nil && p.PollIntervalMs >= 100 && p.PollIntervalMs <= 60000 {
			interval = time.Duration(p.PollIntervalMs) * time.Millisecond
		}
		if failures > 0 {
			interval = min(30*time.Second, interval*time.Duration(1<<min(failures, 5)))
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func validID(id string) bool {
	if len(id) < 1 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
