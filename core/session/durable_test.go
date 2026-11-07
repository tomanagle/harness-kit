package session_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/session"
)

func TestDurableHistoryRequiresPersistentStore(t *testing.T) {
	m := newManager(t, session.Options{})
	info := create(t, m, agent.SessionConfig{})
	collect(t, m, info.ID, "memory only")
	if _, err := m.DurableEventsPage(context.Background(), info.ID, 0, 1); !errors.Is(err, session.ErrDurableHistoryUnavailable) {
		t.Fatalf("memory history is not durable: %v", err)
	}
}

func TestDurableHistorySurvivesColdManagerAndIsBounded(t *testing.T) {
	ctx := context.Background()
	st := historyStore(t, t.TempDir())
	m := newManager(t, session.Options{Store: st})
	info := create(t, m, agent.SessionConfig{})
	collect(t, m, info.ID, "persisted")
	if err := m.Close(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	page, err := m.DurableEventsPage(ctx, info.ID, 0, 2)
	if err != nil || page.Truncated || len(page.Events) != 2 || !page.HasMore {
		t.Fatalf("page: %+v %v", page, err)
	}
	cold := newManager(t, session.Options{Store: st})
	replayed, err := cold.DurableEventsPage(ctx, info.ID, 0, 2)
	if err != nil || !reflect.DeepEqual(page, replayed) {
		t.Fatalf("cold page: %+v %v", replayed, err)
	}
}
