package harnesskit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// SubscribeEvents installs an independent listener. Like Options.OnEvent,
// callbacks run on the read goroutine and must not make blocking RPC calls.
// Cancellation is idempotent; one callback already in flight may finish.
func (c *Client) SubscribeEvents(listener func(HarnessEvent)) func() {
	c.eventMu.Lock()
	if c.eventListeners == nil {
		c.eventListeners = make(map[uint64]func(HarnessEvent))
	}
	c.eventNext++
	id := c.eventNext
	c.eventListeners[id] = listener
	c.eventMu.Unlock()
	return func() { c.eventMu.Lock(); delete(c.eventListeners, id); c.eventMu.Unlock() }
}
func (c *Client) publishEvent(event HarnessEvent) {
	c.eventMu.Lock()
	listeners := make([]func(HarnessEvent), 0, len(c.eventListeners))
	for _, listener := range c.eventListeners {
		listeners = append(listeners, listener)
	}
	c.eventMu.Unlock()
	for _, listener := range listeners {
		copy := event
		copy.Data = append(json.RawMessage(nil), event.Data...)
		listener(copy)
	}
}

// Run starts a turn and waits for its terminal result without retaining deltas.
// It subscribes before sending, recovers terminal events after stream gaps, and
// cancels the accepted turn if ctx is cancelled. Failed agent turns are results;
// transport/admission/replay failures are errors. SubscribeEvents remains usable.
func (c *Client) Run(ctx context.Context, params SessionPromptParams) (TurnCompletedData, error) {
	if err := ctx.Err(); err != nil {
		return TurnCompletedData{}, err
	}
	if params.TurnID == "" {
		var bytes [16]byte
		if _, err := rand.Read(bytes[:]); err != nil {
			return TurnCompletedData{}, err
		}
		params.TurnID = hex.EncodeToString(bytes[:])
	}
	terminal := make(chan HarnessEvent, 1)
	gaps := make(chan uint64, 1)
	var cursor uint64 // Accessed only by the read-goroutine callback.
	stop := c.SubscribeEvents(func(event HarnessEvent) {
		if event.SessionID != params.SessionID {
			return
		}
		if event.Type == "stream.gap" {
			select {
			case gaps <- cursor:
			default:
			}
			return
		}
		if event.Seq > cursor {
			cursor = event.Seq
		}
		if event.TurnID == params.TurnID && event.Type == "turn.completed" {
			select {
			case terminal <- event:
			default:
			}
		}
	})
	defer stop()
	if _, err := c.SessionPrompt(ctx, params); err != nil {
		return TurnCompletedData{}, err
	}
	defer func() {
		if ctx.Err() != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _ = c.SessionCancel(cleanup, SessionIDParams{SessionID: params.SessionID})
		}
	}()
	decode := func(event HarnessEvent) (TurnCompletedData, error) {
		var result TurnCompletedData
		err := json.Unmarshal(event.Data, &result)
		return result, err
	}
	for {
		select {
		case event := <-terminal:
			return decode(event)
		case after := <-gaps:
			for {
				page, err := c.SessionEventsPage(ctx, SessionEventsParams{SessionID: params.SessionID, AfterSeq: after, Limit: 200})
				if err != nil {
					return TurnCompletedData{}, err
				}
				for _, event := range page.Events {
					if event.TurnID == params.TurnID && event.Type == "turn.completed" {
						return decode(event)
					}
				}
				if page.Truncated {
					return TurnCompletedData{}, fmt.Errorf("harness-kit: retained history cannot recover the turn gap")
				}
				if !page.HasMore {
					break
				}
				if page.NextSeq <= after {
					return TurnCompletedData{}, fmt.Errorf("harness-kit: history did not advance")
				}
				after = page.NextSeq
			}
		case <-ctx.Done():
			return TurnCompletedData{}, ctx.Err()
		case <-c.Done():
			select {
			case event := <-terminal:
				return decode(event)
			default:
			}
			err := c.Err()
			if err == nil {
				err = ErrClosed
			}
			return TurnCompletedData{}, err
		}
	}
}

// EncodeOptions turns generated CodexOptions/ClaudeOptions or a custom adapter
// struct into the native option object accepted by the generated methods.
func EncodeOptions(value any) (map[string]json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var options map[string]json.RawMessage
	if err = json.Unmarshal(raw, &options); err != nil {
		return nil, err
	}
	if options == nil {
		return nil, fmt.Errorf("harness-kit: options must be an object")
	}
	return options, nil
}
