package session

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
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
	if s.IdempotencyKey != "" {
		for _, existing := range m.sessions {
			if existing.ProjectName == s.ProjectName && existing.IdempotencyKey == s.IdempotencyKey {
				return ErrIdempotencyConflict
			}
		}
	}
	m.sessions[s.ID] = row
	return nil
}

func (m *memStore) FindByIdempotencyKey(_ context.Context, project, key string) (*Session, error) {
	if key == "" {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range m.sessions {
		if row.ProjectName == project && row.IdempotencyKey == key {
			return RowToSession(row)
		}
	}
	return nil, nil
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
	stored, ok := m.sessions[s.ID]
	if !ok {
		return fmt.Errorf("session %q not found", s.ID)
	}
	// Only a run's mutable fields are written, matching the column list the
	// SQLite store's UPDATE names. What a submission fixed — its prompt, mode,
	// metadata, and the queue columns the backlog operations own — is read back
	// from the stored row, so an ordinary state update along a job's path cannot
	// rewrite the record of what was asked for.
	stored.State = row.State
	stored.Message = row.Message
	stored.Error = row.Error
	stored.PullRequestURL = row.PullRequestURL
	stored.PRRef = row.PRRef
	stored.HeadBranch = row.HeadBranch
	stored.BaseBranch = row.BaseBranch
	stored.CostJSON = row.CostJSON
	stored.Result = row.Result
	stored.UpdatedAt = row.UpdatedAt
	m.sessions[s.ID] = stored
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

	var createdAfter int64
	if !filter.CreatedAfter.IsZero() {
		createdAfter = ToMicros(filter.CreatedAfter)
	}

	var out []*Session
	for _, r := range rows {
		if filter.Project != "" && r.ProjectName != filter.Project {
			continue
		}
		if filter.PRRef != "" && r.PRRef != filter.PRRef {
			continue
		}
		if filter.Mode != "" && r.Mode != filter.Mode {
			continue
		}
		if len(filter.States) > 0 && !slices.Contains(filter.States, State(r.State)) {
			continue
		}
		if filter.ActiveOnly && State(r.State).IsTerminal() {
			continue
		}
		if createdAfter != 0 && r.CreatedAt <= createdAfter {
			continue
		}
		if match, err := metadataMatches(r.MetadataJSON, filter.Metadata); err != nil {
			return nil, err
		} else if !match {
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

// metadataMatches reports whether a row's stored annotations contain every pair
// in want, matched exactly — the Go statement of the EXISTS predicate the SQLite
// store builds, so the conformance suite proves one rule rather than two.
func metadataMatches(metadataJSON string, want map[string]string) (bool, error) {
	if len(want) == 0 {
		return true, nil
	}
	var have map[string]string
	if metadataJSON != "" && metadataJSON != "{}" {
		if err := json.Unmarshal([]byte(metadataJSON), &have); err != nil {
			return false, fmt.Errorf("unmarshal metadata: %w", err)
		}
	}
	for k, v := range want {
		if got, ok := have[k]; !ok || got != v {
			return false, nil
		}
	}
	return true, nil
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

func (m *memStore) ListPending(_ context.Context, q PendingQuery) ([]*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var pending []*Session
	for _, r := range m.sessions {
		if State(r.State) != StatePending {
			continue
		}
		s, err := RowToSession(r)
		if err != nil {
			return nil, err
		}
		pending = append(pending, s)
	}

	// Effective priority with the same aging and clamp as the SQLite store's
	// ORDER BY, so a test that exercises ordering here proves the same rule.
	ceiling := PendingCeiling(pending)
	sort.SliceStable(pending, func(i, j int) bool {
		pi, pj := q.EffectivePriority(pending[i], ceiling), q.EffectivePriority(pending[j], ceiling)
		if pi != pj {
			return pi > pj
		}
		return pending[i].QueuedAt.Before(pending[j].QueuedAt)
	})

	if q.Limit > 0 && len(pending) > q.Limit {
		pending = pending[:q.Limit]
	}
	return pending, nil
}

func (m *memStore) UpdatePendingPriority(_ context.Context, id string, priority int) (int, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.sessions[id]
	if !ok || State(row.State) != StatePending {
		return 0, false, nil
	}
	previous := row.Priority
	row.Priority = priority
	row.UpdatedAt = ToMicros(time.Now())
	m.sessions[id] = row
	return previous, true, nil
}

func (m *memStore) CountPending(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.sessions {
		if State(r.State) == StatePending {
			n++
		}
	}
	return n, nil
}

func (m *memStore) TransitionPending(_ context.Context, id string, to PendingTransition) (bool, PersistedEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.sessions[id]
	if !ok || State(row.State) != StatePending {
		return false, PersistedEvent{}, nil
	}
	sess, err := RowToSession(row)
	if err != nil {
		return false, PersistedEvent{}, err
	}
	sess.State = to.State
	sess.Message = to.Message
	sess.Error = to.Error
	ev, err := m.applyTransitionLocked(sess, time.Now().UTC())
	if err != nil {
		return false, PersistedEvent{}, err
	}
	return true, ev, nil
}

func (m *memStore) RequeueRun(_ context.Context, id string, opts RequeueOpts) (bool, PersistedEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.sessions[id]
	if !ok || !State(row.State).IsRestartable() {
		return false, PersistedEvent{}, nil
	}
	sess, err := RowToSession(row)
	if err != nil {
		return false, PersistedEvent{}, err
	}
	if opts.MaxAttempts > 0 && sess.Attempts >= opts.MaxAttempts {
		return false, PersistedEvent{}, nil
	}
	sess.Attempts++
	sess.State = StatePending
	sess.Message = opts.Message
	sess.Error = ""
	sess.QueuedAt = time.Now().UTC()
	ev, err := m.applyTransitionLocked(sess, time.Now().UTC())
	if err != nil {
		return false, PersistedEvent{}, err
	}
	return true, ev, nil
}

func (m *memStore) ExpirePending(_ context.Context, cutoff time.Time, reason string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoffMicros := ToMicros(cutoff)
	now := time.Now().UTC()
	var ids []string
	for id, row := range m.sessions {
		if State(row.State) != StatePending || row.QueuedAt >= cutoffMicros {
			continue
		}
		sess, err := RowToSession(row)
		if err != nil {
			return nil, err
		}
		sess.State = StateFailed
		sess.Error = reason
		if _, err := m.applyTransitionLocked(sess, now); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func (m *memStore) ReconcileStartup(_ context.Context, opts ReconcileOpts) (ReconcileResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result ReconcileResult
	now := time.Now().UTC()
	for id, row := range m.sessions {
		state := State(row.State)
		if state.IsTerminal() || state == StatePending {
			continue
		}
		sess, err := RowToSession(row)
		if err != nil {
			return ReconcileResult{}, err
		}
		requeue := state.IsRestartable()
		if requeue && opts.MaxAttempts > 0 && sess.Attempts >= opts.MaxAttempts {
			requeue = false
		}
		if requeue {
			sess.Attempts++
			sess.State = StatePending
			sess.Message = opts.RequeueMessage
			sess.Error = ""
			sess.QueuedAt = now
			result.Requeued = append(result.Requeued, id)
		} else {
			sess.State = StateFailed
			sess.Error = opts.FailError
			if opts.MaxAttempts > 0 && sess.Attempts >= opts.MaxAttempts {
				sess.Error = fmt.Sprintf("%s (gave up after %d attempts)", opts.FailError, sess.Attempts)
			}
			result.Failed = append(result.Failed, id)
		}
		if _, err := m.applyTransitionLocked(sess, now); err != nil {
			return ReconcileResult{}, err
		}
	}
	sort.Strings(result.Requeued)
	sort.Strings(result.Failed)
	return result, nil
}

// applyTransitionLocked persists an already-mutated session and appends the
// state_change event recording it. Caller holds m.mu.
func (m *memStore) applyTransitionLocked(sess *Session, now time.Time) (PersistedEvent, error) {
	sess.UpdatedAt = now
	row, err := SessionToRow(sess)
	if err != nil {
		return PersistedEvent{}, err
	}
	m.sessions[sess.ID] = row

	_, payload, _, err := encodeEvent(StateChangeEvent{Session: sess})
	if err != nil {
		return PersistedEvent{}, err
	}
	ev := PersistedEvent{
		SessionID:  sess.ID,
		Seq:        int64(len(m.events[sess.ID])) + 1,
		Kind:       kindStateChange,
		Payload:    payload,
		RecordedAt: now,
	}
	m.events[sess.ID] = append(m.events[sess.ID], ev)
	return ev, nil
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
