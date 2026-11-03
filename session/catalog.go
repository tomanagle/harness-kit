package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/tomanagle/harness-kit/agent"
	"github.com/tomanagle/harness-kit/event"
)

// ListOptions selects stored conversations. Empty filters preserve List's
// original all-session behavior. Tags must all match; title matching is a
// case-insensitive substring. SearchTranscript also searches event payloads.
type ListOptions struct {
	Query            string   `json:"query,omitempty"`
	SearchTranscript bool     `json:"searchTranscript,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	Archived         *bool    `json:"archived,omitempty"`
	Runtime          string   `json:"runtime,omitempty"`
	Cwd              string   `json:"cwd,omitempty"`
	Limit            int      `json:"limit,omitempty"`
}

// Tags reads the canonical tag list stored in metadata["tags"]. Unknown or
// legacy metadata values are ignored rather than preventing session reads.
func Tags(info Info) []string {
	var tags []string
	if json.Unmarshal([]byte(info.Metadata["tags"]), &tags) != nil {
		return nil
	}
	tags, _ = cleanTags(tags)
	return tags
}

func cleanTags(tags []string) ([]string, error) {
	if len(tags) > 64 {
		return nil, fmt.Errorf("%w: at most 64 tags", agent.ErrInvalidConfig)
	}
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.ToLower(strings.Join(strings.Fields(tag), " "))
		if tag == "" || utf8.RuneCountInString(tag) > 64 {
			return nil, fmt.Errorf("%w: tags must contain 1 to 64 characters", agent.ErrInvalidConfig)
		}
		out = append(out, tag)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// Query filters sessions without starting any runtime. Transcript scans retain
// one event and a short search suffix; they never load the whole conversation.
func (m *Manager) Query(ctx context.Context, p ListOptions) ([]Info, error) {
	if p.Limit < 0 || utf8.RuneCountInString(p.Query) > 1024 {
		return nil, fmt.Errorf("%w: invalid list limit or query longer than 1024 characters", agent.ErrInvalidConfig)
	}
	tags, err := cleanTags(p.Tags)
	if err != nil {
		return nil, err
	}
	all, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(all, func(a, b Info) int {
		if c := b.UpdatedAt.Compare(a.UpdatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	query := strings.ToLower(p.Query)
	out := make([]Info, 0)
	for _, info := range all {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if p.Runtime != "" && info.Runtime != p.Runtime || p.Cwd != "" && info.Cwd != p.Cwd || p.Archived != nil && info.Archived != *p.Archived {
			continue
		}
		actual := Tags(info)
		matches := true
		for _, tag := range tags {
			if !slices.Contains(actual, tag) {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(info.Title), query) {
			if !p.SearchTranscript {
				continue
			}
			found, err := m.searchTranscript(ctx, info.ID, query)
			if err != nil {
				return nil, err
			}
			if !found {
				continue
			}
		}
		out = append(out, info)
		if p.Limit > 0 && len(out) >= p.Limit {
			break
		}
	}
	return out, nil
}

var errSearchFound = errors.New("session: search found")

func (m *Manager) searchTranscript(ctx context.Context, id, query string) (bool, error) {
	var suffix, key string
	visit := func(ev event.Event) error {
		var text, nextKey string
		switch d := ev.Data.(type) {
		case event.MessageDeltaData:
			text = d.Text
			nextKey = ev.TurnID + ":" + d.ItemID
		default:
			b, err := json.Marshal(ev.Data)
			if err != nil {
				return err
			}
			text = string(b)
		}
		text = strings.ToLower(text)
		if nextKey == "" || nextKey != key {
			suffix = ""
		}
		joined := suffix + text
		if strings.Contains(joined, query) {
			return errSearchFound
		}
		if len(joined) > len(query) {
			joined = joined[len(joined)-len(query):]
		}
		suffix, key = joined, nextKey
		return nil
	}
	if scanner, ok := m.store.(EventScanner); ok {
		err := scanner.ScanEvents(ctx, id, visit)
		if errors.Is(err, errSearchFound) {
			return true, nil
		}
		return false, err
	}
	var cursor uint64
	for {
		page, err := m.EventsPage(ctx, id, cursor, 128)
		if err != nil {
			return false, err
		}
		for _, ev := range page.Events {
			if err := visit(ev); err != nil {
				if errors.Is(err, errSearchFound) {
					return true, nil
				}
				return false, err
			}
		}
		if !page.HasMore {
			return false, nil
		}
		if page.NextSeq <= cursor {
			return false, errors.New("session: search made no progress")
		}
		cursor = page.NextSeq
	}
}

// Archive sets catalog visibility without stopping or deleting a conversation.
func (m *Manager) Archive(ctx context.Context, id string, archived bool) (Info, error) {
	return m.updateCatalog(ctx, id, func(info *Info) { info.Archived = archived })
}

// SetTags replaces metadata["tags"] with normalized, sorted, unique tags.
// An empty slice clears tags while preserving unrelated host metadata.
func (m *Manager) SetTags(ctx context.Context, id string, tags []string) (Info, error) {
	tags, err := cleanTags(tags)
	if err != nil {
		return Info{}, err
	}
	raw, _ := json.Marshal(tags)
	return m.updateCatalog(ctx, id, func(info *Info) {
		info.Metadata = maps.Clone(info.Metadata)
		if len(tags) == 0 {
			delete(info.Metadata, "tags")
			return
		}
		if info.Metadata == nil {
			info.Metadata = map[string]string{}
		}
		info.Metadata["tags"] = string(raw)
	})
}

func (m *Manager) updateCatalog(ctx context.Context, id string, update func(*Info)) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	ms, ok := m.lookup(id)
	if !ok {
		rec, err := m.store.Get(ctx, id)
		if err != nil {
			return Info{}, err
		}
		info, err := m.storedInfo(ctx, rec.Info)
		if err != nil {
			return Info{}, err
		}
		ms, err = m.newManaged(ctx, info, rec.Config, true)
		if err != nil {
			return Info{}, err
		}
	}
	ms.emitMu.Lock()
	defer ms.emitMu.Unlock()
	ms.mu.Lock()
	old := ms.info
	update(&ms.info)
	ms.mu.Unlock()
	if err := ms.persist(); err != nil {
		ms.mu.Lock()
		ms.info = old
		ms.mu.Unlock()
		return Info{}, err
	}
	ms.updatedLocked("")
	err := ms.persist()
	return ms.snapshot(), err
}
