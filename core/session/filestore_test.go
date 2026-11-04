package session_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/runtimes/fake"
	"github.com/tomanagle/harness-kit/core/session"
)

func TestFileStore(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "nested", "sessions")
	st, err := session.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	recs := []session.Record{
		{Info: session.Info{ID: "ses_b", Runtime: "fake", CreatedAt: now.Add(time.Second), Metadata: map[string]string{"k": "v"}},
			Config: agent.SessionConfig{Cwd: "/tmp", Options: map[string]any{"sandbox": "read-only"}}},
		{Info: session.Info{ID: "ses_a", Runtime: "fake", CreatedAt: now}},
	}
	for _, r := range recs {
		if err := st.Put(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.Get(ctx, "ses_b")
	if err != nil || got.Info.Metadata["k"] != "v" || got.Config.Cwd != "/tmp" {
		t.Fatalf("get: %+v %v", got, err)
	}
	var opts struct{ Sandbox string }
	if err := agent.DecodeOptions(got.Config.Options, &opts); err != nil || opts.Sandbox != "read-only" {
		t.Fatalf("options round trip: %+v %v", got.Config.Options, err)
	}
	// Overwrite is atomic and leaves no temp files.
	recs[1].Info.Turns = 3
	if err := st.Put(ctx, recs[1]); err != nil {
		t.Fatal(err)
	}
	list, err := st.List(ctx)
	if err != nil || len(list) != 2 || list[0].Info.ID != "ses_a" || list[0].Info.Turns != 3 {
		t.Fatalf("list: %+v %v", list, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("expected exactly 2 files, got %d", len(entries))
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dir, "ses_a.json"))
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("record mode %v, want 0600", fi.Mode().Perm())
		}
	}

	// Corrupt files are skipped by List and reported by Get.
	os.WriteFile(filepath.Join(dir, "ses_bad.json"), []byte("{nope"), 0o600)
	if list, _ := st.List(ctx); len(list) != 2 {
		t.Fatalf("corrupt file should be skipped: %d", len(list))
	}
	if _, err := st.Get(ctx, "ses_bad"); err == nil || errors.Is(err, session.ErrNotFound) {
		t.Fatalf("corrupt get: %v", err)
	}

	// IDs cannot escape the directory.
	if err := st.Put(ctx, session.Record{Info: session.Info{ID: "../../etc/x"}}); err == nil {
		t.Fatal("path traversal must be rejected")
	}
	if _, err := st.Get(ctx, "../x"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("traversal get: %v", err)
	}

	if err := st.Delete(ctx, "ses_a"); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "ses_a"); err != nil {
		t.Fatalf("deleting a missing record: %v", err)
	}
	if _, err := st.Get(ctx, "ses_a"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestRestoreAcrossManagers(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	newMgr := func() *session.Manager {
		st, err := session.NewFileStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		return session.NewManager(agent.NewRegistry(fake.New()), session.Options{Store: st})
	}

	// First "process": create a session and run a turn.
	m1 := newMgr()
	info, err := m1.Create(ctx, session.CreateParams{Runtime: "fake", Config: agent.SessionConfig{Cwd: dir}, Metadata: map[string]string{"app": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m1.Restore(ctx, info.ID); err != nil {
		t.Fatalf("restoring a live session returns it: %v", err)
	}
	collect(t, m1, info.ID, "remember the word pineapple please")
	m1.Shutdown(ctx)

	// Second "process": the session is listed and can be restored.
	m2 := newMgr()
	defer m2.Shutdown(ctx)
	list, err := m2.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	stored := list[0]
	if stored.State != session.StateClosed || stored.Turns != 1 || stored.LastStatus != event.TurnStatusCompleted || stored.Title != "remember the word pineapple please" {
		t.Fatalf("stored: %+v", stored)
	}
	restored, err := m2.Restore(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.ID != info.ID || restored.RuntimeSessionID != info.RuntimeSessionID || restored.State != session.StateIdle || restored.Metadata["app"] != "x" || restored.Cwd != dir {
		t.Fatalf("restored: %+v", restored)
	}
	evs, _, _ := m2.Events(info.ID, stored.LastSeq, 1)
	if d := evs[0].Data.(event.SessionStartedData); !d.Resumed {
		t.Fatal("restore should report resumed")
	}
	collect(t, m2, info.ID, "second")
	got, _ := m2.Get(ctx, info.ID)
	if got.Turns != 2 || got.Title != "remember the word pineapple please" {
		t.Fatalf("after second turn: %+v", got)
	}

	// Delete removes the record (and closes the live session).
	if err := m2.Delete(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m2.Get(ctx, info.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := m2.Delete(ctx, info.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("double delete: %v", err)
	}
	if _, err := m2.Restore(ctx, "ses_missing"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("restore missing: %v", err)
	}
}

func TestRestoreRequiresAStartedSession(t *testing.T) {
	ctx := context.Background()
	m := newManager(t, session.Options{})
	info := create(t, m, agent.SessionConfig{})
	m.Close(ctx, info.ID)
	if _, err := m.Restore(ctx, info.ID); !errors.Is(err, session.ErrNotResumable) {
		t.Fatalf("a session with no turns has nothing to resume: %v", err)
	}
}

func TestRename(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	newMgr := func() *session.Manager {
		st, err := session.NewFileStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		return session.NewManager(agent.NewRegistry(fake.New()), session.Options{Store: st})
	}
	m1 := newMgr()

	// A title chosen at creation survives the first prompt.
	named, err := m1.Create(ctx, session.CreateParams{Runtime: "fake", Title: "  Release\tnotes "})
	if err != nil || named.Title != "Release notes" {
		t.Fatalf("create with title: %+v %v", named, err)
	}
	collect(t, m1, named.ID, "draft the notes")
	if got, _ := m1.Get(ctx, named.ID); got.Title != "Release notes" {
		t.Fatalf("prompt replaced the title: %q", got.Title)
	}

	// Renaming a live session, then prompting, keeps the new title.
	auto := create(t, m1, agent.SessionConfig{})
	collect(t, m1, auto.ID, "first prompt")
	before, _ := m1.Get(ctx, auto.ID)
	renamed, err := m1.Rename(ctx, auto.ID, "Refactor\nparser")
	if err != nil || renamed.Title != "Refactor parser" || !renamed.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("rename live: %+v %v", renamed, err)
	}
	collect(t, m1, auto.ID, "second prompt")
	if got, _ := m1.Get(ctx, auto.ID); got.Title != "Refactor parser" || got.Turns != 2 {
		t.Fatalf("after second prompt: %+v", got)
	}

	for _, bad := range []string{"", " \n\t", strings.Repeat("x", session.MaxTitleLength+1)} {
		if _, err := m1.Rename(ctx, auto.ID, bad); !errors.Is(err, session.ErrInvalidTitle) {
			t.Fatalf("rename %q: %v", bad, err)
		}
	}
	if _, err := m1.Create(ctx, session.CreateParams{Runtime: "fake", Title: strings.Repeat("x", session.MaxTitleLength+1)}); !errors.Is(err, session.ErrInvalidTitle) {
		t.Fatalf("create with a long title: %v", err)
	}
	if _, err := m1.Rename(ctx, "ses_missing", "x"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("rename missing: %v", err)
	}
	m1.Shutdown(ctx)

	// A stored session that isn't live can be renamed, and keeps the title
	// when restored.
	m2 := newMgr()
	defer m2.Shutdown(ctx)
	if _, err := m2.Rename(ctx, auto.ID, "Stored rename"); err != nil {
		t.Fatal(err)
	}
	restored, err := m2.Restore(ctx, auto.ID)
	if err != nil || restored.Title != "Stored rename" {
		t.Fatalf("restored: %+v %v", restored, err)
	}
	// Renaming a closed session that is still held in memory updates both
	// what Get returns and the store.
	if err := m2.Close(ctx, auto.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m2.Rename(ctx, auto.ID, "After close"); err != nil {
		t.Fatal(err)
	}
	if got, _ := m2.Get(ctx, auto.ID); got.Title != "After close" {
		t.Fatalf("get after closed rename: %q", got.Title)
	}
	m3 := newMgr()
	defer m3.Shutdown(ctx)
	if got, _ := m3.Get(ctx, auto.ID); got.Title != "After close" {
		t.Fatalf("store after closed rename: %q", got.Title)
	}
}
