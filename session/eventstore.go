package session

import (
	"context"
	"errors"

	"github.com/tomanagle/harness-kit/event"
)

// ErrEventTooLarge means the next event cannot fit within a requested page.
// Increase the byte limit to read it; its sequence is never silently skipped.
var ErrEventTooLarge = errors.New("session: event exceeds page byte limit")

// EventPage is a bounded transcript page. NextSeq is the last returned event's
// sequence, or the supplied cursor for an empty page. HasMore means another
// stored event follows; Truncated reports unavailable history, not pagination.
type EventPage struct {
	Events    []event.Event `json:"events"`
	NextSeq   uint64        `json:"nextSeq"`
	HasMore   bool          `json:"hasMore"`
	Truncated bool          `json:"truncated"`
}

// EventPageStore is an optional bounded-reading extension. MaxBytes bounds
// encoded event JSON bytes, excluding the transport envelope; nil is unlimited.
// A nonpositive limit means no event-count limit. If the next event exceeds
// maxBytes, implementations return ErrEventTooLarge without advancing the cursor.
type EventPageStore interface {
	EventsPage(ctx context.Context, id string, afterSeq uint64, limit int, maxBytes *int64) (EventPage, error)
}

// EventScanner visits saved events in sequence order without retaining the
// whole history. The callback must not call back into the store. Returning an
// error stops the scan. Memory use is bounded by the current event and whatever
// the callback retains, independently of transcript length.
type EventScanner interface {
	ScanEvents(ctx context.Context, id string, visit func(event.Event) error) error
}

// TurnLookup allows durable duplicate-turn checking without a growing in-memory
// set of every historical turn ID.
type TurnLookup interface {
	HasTurn(ctx context.Context, id, turnID string) (bool, error)
}
