package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/agent"
	"github.com/tomanagle/harness-kit/event"
	"github.com/tomanagle/harness-kit/session"
	"github.com/tomanagle/harness-kit/settings"
)

func resourcePatch(t *testing.T, s string) settings.Patch {
	t.Helper()
	var p settings.Patch
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		t.Fatal(err)
	}
	return p
}
func resourceConfigure(t *testing.T, m *session.Manager, s string) settings.Snapshot {
	t.Helper()
	snap, err := m.ConfigureSettings(context.Background(), settings.ConfigureParams{Patch: resourcePatch(t, s)})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}
func eventuallyResource(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("resource policy did not converge")
}

func TestUnifiedSettingsAtomicRevisionAndStartupDefaults(t *testing.T) {
	ctx := context.Background()
	m := newManager(t, session.Options{})
	startup, _ := m.Settings(ctx, settings.Target{})
	startup.Values.Session.Runtime = "fake"
	startup.Values.Session.Model = "initial"
	startup.Values.Session.Effort = "low"
	if err := m.InitializeSettings(ctx, startup.Values); err != nil {
		t.Fatal(err)
	}
	if err := m.InitializeSettings(ctx, startup.Values); !errors.Is(err, settings.ErrRestartRequired) {
		t.Fatalf("repeat init: %v", err)
	}
	info, err := m.Create(ctx, session.CreateParams{})
	if err != nil || info.Model != "initial" {
		t.Fatalf("global defaults: %+v %v", info, err)
	}
	original, _ := m.Settings(ctx, settings.Target{})
	changed, err := m.ConfigureSettings(ctx, settings.ConfigureParams{Patch: resourcePatch(t, `{"session":{"model":"future"},"history":{"maxPageEvents":3}}`), ExpectedRevision: &original.Revision})
	if err != nil || changed.Revision != original.Revision+1 {
		t.Fatalf("update: %+v %v", changed, err)
	}
	if got, _ := m.Get(ctx, info.ID); got.Model != "initial" {
		t.Fatal("global defaults changed an existing session")
	}
	fresh, err := m.Create(ctx, session.CreateParams{})
	if err != nil || fresh.Model != "future" {
		t.Fatalf("new session inheritance: %+v %v", fresh, err)
	}
	if _, err := m.ConfigureSettings(ctx, settings.ConfigureParams{Patch: resourcePatch(t, `{"history":{"maxPageEvents":5}}`), ExpectedRevision: &original.Revision}); !errors.Is(err, settings.ErrConflict) {
		t.Fatalf("stale update: %v", err)
	}
	if _, err := m.ConfigureSettings(ctx, settings.ConfigureParams{Patch: resourcePatch(t, `{"process":{"store":"elsewhere"},"history":{"maxPageEvents":7}}`)}); !errors.Is(err, settings.ErrRestartRequired) {
		t.Fatalf("mixed restart update: %v", err)
	}
	after, _ := m.Settings(ctx, settings.Target{})
	if after.Values.History.MaxPageEvents != 3 || after.Revision != changed.Revision {
		t.Fatal("rejected update partly applied")
	}
	noop := resourceConfigure(t, m, `{"history":{"maxPageEvents":3}}`)
	if noop.Revision != after.Revision {
		t.Fatal("no-op changed revision")
	}
	reset, err := m.ConfigureSettings(ctx, settings.ConfigureParams{Patch: settings.Patch{}, Reset: []string{"session.model"}})
	if err != nil || reset.Values.Session.Model != "initial" {
		t.Fatalf("startup reset: %+v %v", reset, err)
	}
}

func TestUnifiedSessionAndTurnSettings(t *testing.T) {
	ctx := context.Background()
	m := newManager(t, session.Options{})
	info := create(t, m, agent.SessionConfig{Model: "base", Effort: "low", Cwd: "/tmp"})
	scope := settings.Target{SessionID: info.ID}
	before, _ := m.Settings(ctx, scope)
	updated, err := m.ConfigureSettings(ctx, settings.ConfigureParams{Target: scope, ExpectedRevision: &before.Revision, Patch: resourcePatch(t, `{"session":{"model":"session","effort":"high"},"memory":{"pinned":true}}`)})
	if err != nil || !updated.Values.Memory.Pinned || updated.Values.Session.Model != "session" {
		t.Fatalf("scoped update: %+v %v", updated, err)
	}
	if _, err := m.ConfigureSettings(ctx, settings.ConfigureParams{Target: scope, ExpectedRevision: &before.Revision, Patch: resourcePatch(t, `{"session":{"model":"stale"}}`)}); !errors.Is(err, settings.ErrConflict) {
		t.Fatalf("scoped stale update: %v", err)
	}
	if _, err := m.ConfigureSettings(ctx, settings.ConfigureParams{Target: scope, Patch: resourcePatch(t, `{"session":{"cwd":"/different","model":"bad"}}`)}); !errors.Is(err, settings.ErrNewSessionRequired) {
		t.Fatalf("immutable: %v", err)
	}
	evs := []event.Event{}
	for ev, err := range m.Stream(ctx, info.ID, session.PromptParams{Input: agent.Text("one"), Settings: resourcePatch(t, `{"session":{"model":"turn","effort":"low"}}`)}) {
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev)
	}
	if got := evs[0].Data.(event.TurnStartedData).Config; got.Model != "turn" || got.Effort != "low" {
		t.Fatalf("turn selection: %+v", got)
	}
	current, _ := m.Settings(ctx, scope)
	if current.Values.Session.Model != "session" {
		t.Fatal("turn changed session defaults")
	}
	reset, err := m.ConfigureSettings(ctx, settings.ConfigureParams{Target: scope, Patch: settings.Patch{}, Reset: []string{"session.model", "session.effort", "memory.pinned"}})
	if err != nil || reset.Values.Memory.Pinned || reset.Values.Session.Model != "base" || reset.Values.Session.Effort != "low" {
		t.Fatalf("scoped reset: %+v %v", reset, err)
	}
	raw, _ := json.Marshal(reset.Values)
	var full settings.Patch
	_ = json.Unmarshal(raw, &full)
	if _, err := m.ConfigureSettings(ctx, settings.ConfigureParams{Target: scope, Patch: full}); err != nil {
		t.Fatalf("unchanged full snapshot: %v", err)
	}
	if _, err := m.Prompt(ctx, info.ID, session.PromptParams{Input: agent.Text("bad"), Config: &agent.ConfigPatch{}, Settings: settings.Patch{}}); !errors.Is(err, settings.ErrInvalid) {
		t.Fatalf("dual prompt settings accepted: %v", err)
	}
}

func TestSettingsNotificationsAreIndependentAndSignalResync(t *testing.T) {
	ctx := context.Background()
	m := newManager(t, session.Options{})
	info := create(t, m, agent.SessionConfig{})
	first, cancelFirst := m.SubscribeSettings()
	defer cancelFirst()
	second, cancelSecond := m.SubscribeSettings()
	defer cancelSecond()
	resourceConfigure(t, m, `{"history":{"maxPageEvents":7}}`)
	a := <-first
	b := <-second
	a.Values.Process.Runtimes[0] = "mutated"
	f := a.Fields["session.model"]
	f.Scopes[0] = "mutated"
	a.Fields["session.model"] = f
	if b.Values.Process.Runtimes[0] == "mutated" || b.Fields["session.model"].Scopes[0] == "mutated" {
		t.Fatal("notifications share mutable data")
	}
	resourceConfigure(t, m, `{"history":{"maxPageEvents":8}}`)
	model := "next"
	if _, err := m.Configure(ctx, info.ID, agent.ConfigPatch{Model: &model}); err != nil {
		t.Fatal(err)
	}
	newest := <-first
	if !newest.Resync || newest.Target.SessionID != info.ID {
		t.Fatalf("lost cross-target update not signaled: %+v", newest)
	}
	snapshot, _ := m.Settings(ctx, settings.Target{})
	if snapshot.Values.History.MaxPageEvents != 8 {
		t.Fatal("could not resync")
	}
}

func TestSuspendResumeAndEvictPreserveHistory(t *testing.T) {
	ctx := context.Background()
	st := historyStore(t, t.TempDir())
	m := newManager(t, session.Options{Store: st})
	info := create(t, m, agent.SessionConfig{Model: "saved"})
	collect(t, m, info.ID, "a saved conversation")
	before, _, _ := m.Events(info.ID, 0, 0)
	if err := m.Evict(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	if got := m.MemoryStats(); got.CachedEvents != 0 {
		t.Fatalf("cache retained after eviction: %+v", got)
	}
	page, err := m.EventsPage(ctx, info.ID, 0, 0)
	if err != nil || !reflect.DeepEqual(before, page.Events) {
		t.Fatalf("cold history: %+v %v", page, err)
	}
	if err := m.Suspend(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	dormant, _ := m.Get(ctx, info.ID)
	if dormant.State != session.StateSuspended || dormant.ID != info.ID {
		t.Fatalf("suspended info: %+v", dormant)
	}
	if got := m.MemoryStats(); got.ResidentSessions != 0 || got.CachedEvents != 0 {
		t.Fatalf("suspended resources: %+v", got)
	}
	if _, err := m.Prompt(ctx, info.ID, session.PromptParams{Input: agent.Text("requires resume")}); err == nil {
		t.Fatal("resumed without opt-in")
	}
	resourceConfigure(t, m, `{"memory":{"resumeOnPrompt":true}}`)
	turn, err := m.Prompt(ctx, info.ID, session.PromptParams{Input: agent.Text("again")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	after, _, err := m.Events(info.ID, 0, 0)
	if err != nil || len(after) <= len(before) {
		t.Fatalf("restored events: %v", err)
	}
	for i, ev := range after {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("sequence changed after resume: %d %d", i, ev.Seq)
		}
	}
}

func TestSuspendEmptySessionAndIdlePolicy(t *testing.T) {
	ctx := context.Background()
	m := newManager(t, session.Options{Store: historyStore(t, t.TempDir())})
	info := create(t, m, agent.SessionConfig{})
	if err := m.Suspend(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Restore(ctx, info.ID); err != nil {
		t.Fatalf("empty suspended session cannot cold start: %v", err)
	}
	resourceConfigure(t, m, `{"memory":{"suspendAfterIdleMs":0}}`)
	eventuallyResource(t, func() bool { got, _ := m.Get(ctx, info.ID); return got.State == session.StateSuspended })
}

func TestLiveLimitsProtectBusyAndPinnedSessions(t *testing.T) {
	ctx := context.Background()
	m := newManager(t, session.Options{Store: historyStore(t, t.TempDir())})
	info := create(t, m, agent.SessionConfig{})
	if _, err := m.Pin(ctx, info.ID, true); err != nil {
		t.Fatal(err)
	}
	collect(t, m, info.ID, "pin the transcript")
	before := m.MemoryStats().CachedEvents
	resourceConfigure(t, m, `{"memory":{"maxCacheBytes":0,"maxSessionCacheBytes":0,"maxCachedSessions":0}}`)
	if m.MemoryStats().CachedEvents != before {
		t.Fatal("pinned cache shrunk")
	}
	if err := m.Suspend(ctx, info.ID); !errors.Is(err, session.ErrBusy) {
		t.Fatalf("suspended pinned session: %v", err)
	}
	if _, err := m.Pin(ctx, info.ID, false); err != nil {
		t.Fatal(err)
	}
	eventuallyResource(t, func() bool { return m.MemoryStats().CachedEvents == 0 })
	// Active turns and approvals are never cancelled to meet a lower budget.
	resourceConfigure(t, m, `{"memory":{"maxCacheBytes":null,"maxSessionCacheBytes":null,"maxCachedSessions":null}}`)
	turn, err := m.Prompt(ctx, info.ID, session.PromptParams{Input: agent.Text("[approve] busy")})
	if err != nil {
		t.Fatal(err)
	}
	eventuallyResource(t, func() bool { return len(m.PendingApprovals(info.ID)) > 0 })
	if err := m.Suspend(ctx, info.ID); !errors.Is(err, session.ErrBusy) {
		t.Fatalf("suspended running session: %v", err)
	}
	resourceConfigure(t, m, `{"memory":{"suspendAfterIdleMs":0,"maxCacheBytes":0}}`)
	select {
	case <-turn.Done():
		t.Fatal("policy cancelled an active turn")
	default:
	}
	if err := m.Cancel(info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	eventuallyResource(t, func() bool { got, _ := m.Get(ctx, info.ID); return got.State == session.StateSuspended })
}

type unavailableEventStore struct{ *session.FileStore }

func (s unavailableEventStore) AppendEvent(context.Context, event.Event) error {
	return errors.New("disk unavailable")
}
func TestFailedPersistenceNeverEvicted(t *testing.T) {
	ctx := context.Background()
	st := unavailableEventStore{historyStore(t, t.TempDir())}
	m := newManager(t, session.Options{Store: st, EventLogSize: 2, MaxClosedSessions: 1})
	info := create(t, m, agent.SessionConfig{})
	collect(t, m, info.ID, "must retain failed writes")
	resourceConfigure(t, m, `{"memory":{"maxCacheBytes":0,"maxClosedSessions":0,"suspendAfterIdleMs":0}}`)
	if err := m.Evict(ctx, info.ID); err == nil {
		t.Fatal("evicted unsaved events")
	}
	if err := m.Suspend(ctx, info.ID); err == nil {
		t.Fatal("suspended unsaved events")
	}
	if err := m.Close(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	events, _, err := m.Events(info.ID, 0, 0)
	if err != nil || len(events) < 5 {
		t.Fatalf("failed events disappeared: count %d err %v", len(events), err)
	}
	var transcript strings.Builder
	for _, ev := range events {
		if d, ok := ev.Data.(event.MessageDeltaData); ok {
			transcript.WriteString(d.Text)
		}
	}
	if !strings.Contains(transcript.String(), "must retain failed writes") {
		t.Fatalf("lost output: %q", transcript.String())
	}
	if m.MemoryStats().UnsavedEvents == 0 {
		t.Fatal("closed session recovery state was removed")
	}
}

func TestMemoryPolicyRequiresEventStoreAndPagingIsBounded(t *testing.T) {
	ctx := context.Background()
	memory := newManager(t, session.Options{})
	if _, err := memory.ConfigureSettings(ctx, settings.ConfigureParams{Patch: resourcePatch(t, `{"memory":{"evictAfterIdleMs":10}}`)}); !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("lossy eviction accepted: %v", err)
	}
	m := newManager(t, session.Options{Store: historyStore(t, t.TempDir())})
	info := create(t, m, agent.SessionConfig{})
	collect(t, m, info.ID, "read this through pages")
	resourceConfigure(t, m, `{"history":{"maxPageEvents":2}}`)
	var cursor uint64
	var all []event.Event
	for {
		page, err := m.EventsPage(ctx, info.ID, cursor, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Events) > 2 {
			t.Fatal("page exceeded live count limit")
		}
		all = append(all, page.Events...)
		if !page.HasMore {
			break
		}
		if page.NextSeq <= cursor {
			t.Fatal("page failed to advance")
		}
		cursor = page.NextSeq
	}
	want, _, _ := m.Events(info.ID, 0, 0)
	if !reflect.DeepEqual(all, want) {
		t.Fatal("paged replay changed transcript")
	}
	resourceConfigure(t, m, `{"history":{"maxPageBytes":1}}`)
	if _, err := m.EventsPage(ctx, info.ID, 0, 0); !errors.Is(err, session.ErrEventTooLarge) {
		t.Fatalf("oversized event not reported: %v", err)
	}
}

func TestExplicitCloseOfSuspendedSessionDoesNotAutoResume(t *testing.T) {
	ctx := context.Background()
	m := newManager(t, session.Options{Store: historyStore(t, t.TempDir())})
	info := create(t, m, agent.SessionConfig{})
	collect(t, m, info.ID, "close after suspend")
	if err := m.Suspend(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	resourceConfigure(t, m, `{"memory":{"resumeOnPrompt":true}}`)
	if err := m.Close(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	got, err := m.Get(ctx, info.ID)
	if err != nil || got.State != session.StateClosed {
		t.Fatalf("close did not persist: %+v %v", got, err)
	}
	if _, err := m.Prompt(ctx, info.ID, session.PromptParams{Input: agent.Text("closed")}); !errors.Is(err, session.ErrClosed) {
		t.Fatalf("closed session resumed: %v", err)
	}
}

func TestEmptyEvictedMemoryPageReportsGapWithoutLooping(t *testing.T) {
	ctx := context.Background()
	m := newManager(t, session.Options{})
	info := create(t, m, agent.SessionConfig{})
	collect(t, m, info.ID, "only memory")
	resourceConfigure(t, m, `{"memory":{"eventLogSize":0}}`)
	page, err := m.EventsPage(ctx, info.ID, 0, 0)
	if err != nil || len(page.Events) != 0 || page.HasMore || !page.Truncated {
		t.Fatalf("empty gap must terminate: %+v %v", page, err)
	}
}

func TestSettingsRevisionIgnoresStreamingAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	m := newManager(t, session.Options{Store: historyStore(t, dir)})
	info := create(t, m, agent.SessionConfig{Model: "before"})
	scope := settings.Target{SessionID: info.ID}
	before, err := m.Settings(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := m.Prompt(ctx, info.ID, session.PromptParams{Input: agent.Text("[approve] changing settings during a turn")})
	if err != nil {
		t.Fatal(err)
	}
	eventuallyResource(t, func() bool { return len(m.PendingApprovals(info.ID)) > 0 })
	during, err := m.ConfigureSettings(ctx, settings.ConfigureParams{Target: scope, ExpectedRevision: &before.Revision, Patch: resourcePatch(t, `{"session":{"model":"after"}}`)})
	if err != nil || during.Revision != before.Revision+1 {
		t.Fatalf("streaming invalidated configuration revision: %+v %v", during, err)
	}
	if err := m.Cancel(info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	finished, _ := m.Settings(ctx, scope)
	if finished.Revision != during.Revision {
		t.Fatal("turn completion changed settings revision")
	}
	if _, err := m.ConfigureSettings(ctx, settings.ConfigureParams{Target: scope, ExpectedRevision: &before.Revision, Patch: resourcePatch(t, `{"session":{"model":"stale"}}`)}); !errors.Is(err, settings.ErrConflict) {
		t.Fatalf("stale edit accepted: %v", err)
	}
	if err := m.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	restarted := newManager(t, session.Options{Store: historyStore(t, dir)})
	recovered, err := restarted.Settings(ctx, scope)
	if err != nil || recovered.Revision != during.Revision || recovered.Values.Session.Model != "after" {
		t.Fatalf("settings revision lost on restart: %+v %v", recovered, err)
	}
}
