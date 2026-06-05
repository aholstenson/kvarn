package session

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// memStore is an in-memory Store implementation used in tests. It round-trips
// sessions through the codec row helpers so it exercises the same encoding path
// as the SQLite store.
type memStore struct {
	mu       sync.Mutex
	sessions map[string]Row
	events   map[string][]PersistedEvent
}

// newMemStore creates an empty in-memory store.
func newMemStore() *memStore {
	return &memStore{
		sessions: make(map[string]Row),
		events:   make(map[string][]PersistedEvent),
	}
}

// NewMemStore returns an in-memory Store. Exported for the Store conformance
// suite and other tests that need a Store without a Manager.
func NewMemStore() Store { return newMemStore() }

func (m *memStore) CreateSession(_ context.Context, s *Session) error {
	row, err := SessionToRow(s)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[s.ID]; ok {
		return fmt.Errorf("session %q already exists", s.ID)
	}
	m.sessions[s.ID] = row
	return nil
}

func (m *memStore) GetSession(_ context.Context, id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %q not found", id)
	}
	return RowToSession(row)
}

func (m *memStore) UpdateSession(_ context.Context, s *Session) error {
	row, err := SessionToRow(s)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[s.ID]; !ok {
		return fmt.Errorf("session %q not found", s.ID)
	}
	m.sessions[s.ID] = row
	return nil
}

func (m *memStore) ListSessions(_ context.Context, filter SessionFilter) ([]*Session, error) {
	m.mu.Lock()
	rows := make([]Row, 0, len(m.sessions))
	for _, r := range m.sessions {
		rows = append(rows, r)
	}
	m.mu.Unlock()

	// Order by (created_at DESC, id DESC) to match the SQLite indexes and give
	// keyset pagination a total order.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt != rows[j].CreatedAt {
			return rows[i].CreatedAt > rows[j].CreatedAt
		}
		return rows[i].ID > rows[j].ID
	})

	var cursorMicros int64
	hasCursor := !filter.AfterCreatedAt.IsZero() || filter.AfterID != ""
	if hasCursor {
		cursorMicros = ToMicros(filter.AfterCreatedAt)
	}

	var out []*Session
	for _, r := range rows {
		if filter.Project != "" && r.ProjectName != filter.Project {
			continue
		}
		if filter.ActiveOnly && State(r.State).IsTerminal() {
			continue
		}
		if hasCursor {
			// Strictly after the cursor in DESC order: (created, id) < cursor.
			if r.CreatedAt > cursorMicros || (r.CreatedAt == cursorMicros && r.ID >= filter.AfterID) {
				continue
			}
		}
		s, err := RowToSession(r)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, nil
}

func (m *memStore) AppendEvent(_ context.Context, sessionID, kind string, payload []byte) (PersistedEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[sessionID]; !ok {
		return PersistedEvent{}, fmt.Errorf("session %q not found", sessionID)
	}
	seq := int64(len(m.events[sessionID])) + 1
	ev := PersistedEvent{
		SessionID:  sessionID,
		Seq:        seq,
		Kind:       kind,
		Payload:    append([]byte(nil), payload...),
		RecordedAt: time.Now().UTC(),
	}
	m.events[sessionID] = append(m.events[sessionID], ev)
	return ev, nil
}

func (m *memStore) ListEvents(_ context.Context, sessionID string, afterSeq int64, limit int) ([]PersistedEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []PersistedEvent
	for _, ev := range m.events[sessionID] {
		if ev.Seq <= afterSeq {
			continue
		}
		cp := ev
		cp.Payload = append([]byte(nil), ev.Payload...)
		out = append(out, cp)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memStore) MaxSeq(_ context.Context, sessionID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(len(m.events[sessionID])), nil
}

func (m *memStore) ReconcileNonTerminal(_ context.Context, reason string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	now := time.Now().UTC()
	for id, row := range m.sessions {
		if State(row.State).IsTerminal() {
			continue
		}
		row.State = string(StateFailed)
		row.Error = reason
		row.UpdatedAt = ToMicros(now)
		m.sessions[id] = row

		s, err := RowToSession(row)
		if err != nil {
			return nil, err
		}
		_, payload, _, err := encodeEvent(StateChangeEvent{Session: s})
		if err != nil {
			return nil, err
		}
		seq := int64(len(m.events[id])) + 1
		m.events[id] = append(m.events[id], PersistedEvent{
			SessionID:  id,
			Seq:        seq,
			Kind:       kindStateChange,
			Payload:    payload,
			RecordedAt: now,
		})
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func (m *memStore) PruneTerminalBefore(_ context.Context, cutoff time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoffMicros := ToMicros(cutoff)
	n := 0
	for id, row := range m.sessions {
		if !State(row.State).IsTerminal() {
			continue
		}
		if row.CreatedAt >= cutoffMicros {
			continue
		}
		delete(m.sessions, id)
		delete(m.events, id) // events cascade
		n++
	}
	return n, nil
}

func (m *memStore) Close() error { return nil }
