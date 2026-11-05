package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/hooks"
	"github.com/tomanagle/harness-kit/core/logging"
	"github.com/tomanagle/harness-kit/core/permission"
	"github.com/tomanagle/harness-kit/core/settings"
)

// Errors returned by the Manager. They can be matched with errors.Is.
var (
	ErrRuntimeNotFound = errors.New("session: runtime not registered")
	ErrBusy            = errors.New("session: a turn is already running")
	ErrClosed          = errors.New("session: closed")
	ErrDuplicateTurn   = errors.New("session: turn id already used")
	ErrUnsafe          = errors.New("session: configuration disables runtime safety checks")
	ErrNotResumable    = errors.New("session: nothing to resume (the runtime never started this session)")
	ErrInvalidTitle    = errors.New("session: invalid title")
)

// MaxTitleLength is the longest title, in characters, that Rename and
// CreateParams.Title accept.
const MaxTitleLength = 200

// Manager owns sessions across all registered runtimes. It is safe for
// concurrent use.
type Manager struct {
	hookMu         sync.Mutex
	hookAdmissions map[string]context.CancelFunc
	reg            *agent.Registry
	opts           Options
	store          Store
	broker         *permission.Broker
	bus            *event.Bus
	log            *slog.Logger
	ownsLogs       bool

	baseCtx    context.Context
	baseCancel context.CancelFunc

	// lifecycleMu serialises restore, rename and delete, including dormant sessions.
	lifecycleMu   sync.Mutex
	gitOperations map[string]gitReservation // protected by lifecycleMu; keyed by Git directory
	mu            sync.Mutex
	sessions      map[string]*managed
	closedOrder   []*managed
	turns         sync.WaitGroup

	settingsMu          sync.RWMutex
	settingsChangeMu    sync.Mutex
	settings            settings.Values
	initialSettings     settings.Values
	settingsRevision    uint64
	settingsSubscribers map[chan settings.Snapshot]struct{}
	settingsInitialized bool
	resourceWake        chan struct{}
	resourceDone        chan struct{}
}

// NewManager returns a Manager for the runtimes in reg.
func NewManager(reg *agent.Registry, opts Options) *Manager {
	if opts.Hooks == nil {
		opts.Hooks = hooks.NewRegistry()
	}
	if opts.Store == nil {
		opts.Store = NewMemoryStore()
	}
	if opts.EventLogSize <= 0 {
		opts.EventLogSize = 2048
	}
	if opts.MaxClosedSessions <= 0 {
		opts.MaxClosedSessions = 64
	}
	ownsLogs := opts.Logs == nil
	if opts.Logs == nil {
		var next slog.Handler
		if opts.Logger != nil {
			next = opts.Logger.Handler()
		}
		opts.Logs = logging.New(opts.LogLevel, next, logging.DefaultConfig())
	}
	opts.LogLevel = opts.Logs.Level()
	if !ownsLogs && opts.Logger != nil {
		opts.Logger = slog.New(opts.Logs.WithDestination(opts.Logger.Handler()))
	} else {
		opts.Logger = slog.New(opts.Logs)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		reg:                 reg,
		opts:                opts,
		store:               opts.Store,
		broker:              permission.NewBroker(),
		bus:                 event.NewBus(),
		log:                 opts.Logger,
		ownsLogs:            ownsLogs,
		baseCtx:             ctx,
		baseCancel:          cancel,
		sessions:            make(map[string]*managed),
		settingsSubscribers: make(map[chan settings.Snapshot]struct{}),
		resourceWake:        make(chan struct{}, 1),
		resourceDone:        make(chan struct{}),
	}
	m.initSettings()
	go m.resourceLoop()
	return m
}

// Registry returns the manager's runtime registry.
func (m *Manager) Registry() *agent.Registry { return m.reg }

// Cancel requests interruption of the active turn or requested compaction.
// The operation keeps its slot until the runtime confirms it has stopped.
func (m *Manager) Cancel(sessionID string) error {
	preparing := m.cancelHookAdmission(sessionID)
	ms, ok := m.lookup(sessionID)
	if !ok {
		if preparing {
			return nil
		}
		return ErrNotFound
	}
	ms.mu.Lock()
	t := ms.turn
	c := ms.compaction
	nativeOnly := t == nil && c == nil && ms.nativeCompaction != nil
	ms.mu.Unlock()
	if nativeOnly {
		return fmt.Errorf("%w: runtime-initiated idle compaction has no cancellable request; close the session to stop its runtime", agent.ErrUnsupported)
	}
	if t != nil {
		t.cancel()
	}
	if c != nil {
		c.cancel()
	}
	return nil
}

// Close cancels active operations, releases the runtime session and emits
// session.closed. Closing a closed session is a no-op.
func (m *Manager) Close(ctx context.Context, sessionID string) error {
	m.cancelHookAdmission(sessionID)
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	ms, ok := m.lookup(sessionID)
	if !ok {
		rec, err := m.store.Get(ctx, sessionID)
		if err != nil {
			return err
		}
		if rec.Info.State != StateSuspended {
			return nil
		}
		ms, err = m.newManaged(ctx, rec.Info, rec.Config, true)
		if err != nil {
			return err
		}
		m.mu.Lock()
		m.sessions[sessionID] = ms
		m.mu.Unlock()
	}
	return ms.close(ctx, "closed")
}

// Shutdown cancels all turns and closes all sessions.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.baseCancel()
	select {
	case <-m.resourceDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	m.mu.Lock()
	all := make([]*managed, 0, len(m.sessions))
	for _, ms := range m.sessions {
		all = append(all, ms)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	errs := make([]error, len(all))
	for i, ms := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.lifecycleMu.Lock()
			defer m.lifecycleMu.Unlock()
			errs[i] = ms.close(ctx, "shutdown")
		}()
	}
	wg.Wait()
	done := make(chan struct{})
	go func() { m.turns.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		errs = append(errs, ctx.Err())
	}
	if m.ownsLogs {
		m.opts.Logs.Close()
	}
	return errors.Join(errs...)
}

func (m *Manager) lookup(id string) (*managed, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, ok := m.sessions[id]
	return ms, ok
}

func (m *Manager) retire(ms *managed) {
	m.mu.Lock()
	m.closedOrder = append(m.closedOrder, ms)
	m.mu.Unlock()
	m.trimClosed(m.memoryPolicy().MaxClosedSessions)
}

// trimClosed respects pins and unsaved recovery payloads. A target may be
// exceeded until those protected records become eligible for reclamation.
func (m *Manager) trimClosed(limit int) {
	m.mu.Lock()
	all := slices.Clone(m.closedOrder)
	count := len(all)
	m.mu.Unlock()
	for _, old := range all {
		if count <= limit {
			break
		}
		old.emitMu.Lock()
		old.mu.Lock()
		safe := !old.info.Pinned && !old.eventWriteFailed && !old.recordWriteFailed
		old.mu.Unlock()
		if safe {
			m.forgetManaged(old)
			count--
		}
		old.emitMu.Unlock()
	}
}

var _ agent.Handler = (*managed)(nil)

// NewID returns prefix followed by 24 random hex characters.
func NewID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}
