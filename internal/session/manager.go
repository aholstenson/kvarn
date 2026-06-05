package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aholstenson/kvarn/internal/agent/cost"
)

// liveBuffer is the per-subscriber output channel capacity.
const liveBuffer = 64

// maxPending bounds how many durable events may queue for a subscriber while it
// is behind. Exceeding it triggers disconnect-on-lag: the subscriber is closed
// rather than silently dropping a durable event (which would create an
// undetectable history gap). The client reconnects via Watch(fromSeq) and
// replays the gap from the store, the source of truth.
const maxPending = 64

// subscriber is a single live watcher. Its output channel ch has exactly one
// writer and one closer: the feeder goroutine. The hub enqueues events into
// pending (under hub.mu) and wakes the feeder via notify; the feeder drains
// pending to ch in order. This keeps channel ownership unambiguous.
type subscriber struct {
	id     string
	ctx    context.Context
	ch     chan WatchEvent
	notify chan struct{} // buffered(1); poked when pending grows

	// Guarded by hub.mu:
	nextSeq int64          // smallest durable seq not yet enqueued
	pending []WatchEvent   // events awaiting the feeder, in seq order
	dead    chan struct{}  // closed on lag-disconnect to unblock a stuck send
	closed  bool           // no further enqueues permitted
}

type hub struct {
	mu   sync.Mutex
	subs map[string][]*subscriber
}

// manager owns the in-memory pub/sub hub and delegates all persistence to a
// Store, layering replay + reconnect-from-cursor on top of the live stream.
type manager struct {
	store Store
	hub   hub
}

// NewManager creates a session Manager backed by the given Store.
func NewManager(store Store) *manager {
	return &manager{
		store: store,
		hub:   hub{subs: make(map[string][]*subscriber)},
	}
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (m *manager) Create(ctx context.Context, projectName, prompt, mode string) (*Session, error) {
	id, err := generateID()
	if err != nil {
		return nil, fmt.Errorf("generate session id: %w", err)
	}
	now := time.Now()
	s := &Session{
		ID:          id,
		ProjectName: projectName,
		Prompt:      prompt,
		Mode:        mode,
		State:       StatePending,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := m.store.CreateSession(ctx, s); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return copySession(s), nil
}

func (m *manager) Get(ctx context.Context, id string) (*Session, error) {
	return m.store.GetSession(ctx, id)
}

func (m *manager) List(ctx context.Context, filter SessionFilter) ([]*Session, error) {
	return m.store.ListSessions(ctx, filter)
}

func (m *manager) UpdateState(ctx context.Context, id string, state State, message string) error {
	s, err := m.store.GetSession(ctx, id)
	if err != nil {
		return err
	}
	s.State = state
	s.Message = message
	s.UpdatedAt = time.Now()
	if err := m.store.UpdateSession(ctx, s); err != nil {
		return err
	}
	return m.persistAndBroadcast(ctx, id, StateChangeEvent{Session: copySession(s)})
}

func (m *manager) Fail(ctx context.Context, id string, failErr error) error {
	s, err := m.store.GetSession(ctx, id)
	if err != nil {
		return err
	}
	s.State = StateFailed
	s.Error = failErr.Error()
	s.UpdatedAt = time.Now()
	if err := m.store.UpdateSession(ctx, s); err != nil {
		return err
	}
	return m.persistAndBroadcast(ctx, id, StateChangeEvent{Session: copySession(s)})
}

func (m *manager) UpdateCost(ctx context.Context, id string, report cost.Report) error {
	s, err := m.store.GetSession(ctx, id)
	if err != nil {
		return err
	}
	s.Cost = report
	s.UpdatedAt = time.Now()
	return m.store.UpdateSession(ctx, s)
}

func (m *manager) SetPullRequest(ctx context.Context, id, url string, number int, branch string) error {
	s, err := m.store.GetSession(ctx, id)
	if err != nil {
		return err
	}
	s.PullRequestURL = url
	s.UpdatedAt = time.Now()
	if err := m.store.UpdateSession(ctx, s); err != nil {
		return err
	}
	return m.persistAndBroadcast(ctx, id, PullRequestEvent{
		SessionID: id,
		URL:       url,
		Number:    number,
		Branch:    branch,
	})
}

func (m *manager) EmitEvent(ctx context.Context, id string, event Event) error {
	// Verify the session exists so callers get a clear error rather than a
	// silently-dropped event.
	if _, err := m.store.GetSession(ctx, id); err != nil {
		return err
	}
	return m.persistAndBroadcast(ctx, id, event)
}

// persistAndBroadcast persists durable events and enqueues every event to live
// subscribers. The store append and the enqueue happen under hub.mu so the seq
// assigned by the store equals the seq broadcast, in order, with no
// concurrently-registering Watch able to interleave.
func (m *manager) persistAndBroadcast(ctx context.Context, id string, e Event) error {
	kind, payload, durable, err := encodeEvent(e)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}

	m.hub.mu.Lock()
	defer m.hub.mu.Unlock()

	seq := int64(0)
	if durable {
		pe, err := m.store.AppendEvent(ctx, id, kind, payload)
		if err != nil {
			return fmt.Errorf("append event: %w", err)
		}
		seq = pe.Seq
	}
	m.broadcastLocked(id, WatchEvent{Seq: seq, Event: e})
	return nil
}

// broadcastLocked enqueues we into each live subscriber's pending buffer. Must
// be called with hub.mu held.
func (m *manager) broadcastLocked(id string, we WatchEvent) {
	for _, sub := range m.hub.subs[id] {
		if sub.closed {
			continue
		}
		if we.Seq == 0 {
			// Ephemeral: best-effort. Drop rather than disconnect when the
			// subscriber is already behind.
			if len(sub.pending) < maxPending {
				sub.pending = append(sub.pending, we)
				poke(sub)
			}
			continue
		}
		// Durable: dedup against what the subscriber already has, then enqueue.
		if we.Seq < sub.nextSeq {
			continue
		}
		if len(sub.pending) >= maxPending {
			// Disconnect-on-lag: closing is safer than dropping a durable event.
			sub.closed = true
			close(sub.dead)
			continue
		}
		sub.pending = append(sub.pending, we)
		sub.nextSeq = we.Seq + 1
		poke(sub)
	}
}

func poke(sub *subscriber) {
	select {
	case sub.notify <- struct{}{}:
	default:
	}
}

func (m *manager) Watch(ctx context.Context, id string, fromSeq int64) (<-chan WatchEvent, error) {
	sess, err := m.store.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	terminal := sess.State.IsTerminal()

	m.hub.mu.Lock()
	backlogMax, err := m.store.MaxSeq(ctx, id)
	if err != nil {
		m.hub.mu.Unlock()
		return nil, err
	}
	sub := &subscriber{
		id:      id,
		ctx:     ctx,
		ch:      make(chan WatchEvent, liveBuffer),
		notify:  make(chan struct{}, 1),
		nextSeq: backlogMax + 1,
		dead:    make(chan struct{}),
	}
	if !terminal {
		m.hub.subs[id] = append(m.hub.subs[id], sub)
	}
	m.hub.mu.Unlock()

	go m.feed(sub, fromSeq, backlogMax, terminal)
	return sub.ch, nil
}

// feed replays history with seq in (fromSeq, backlogMax], then streams live
// events drained from the subscriber's pending buffer. It is the sole writer
// and closer of sub.ch.
func (m *manager) feed(sub *subscriber, fromSeq, backlogMax int64, terminal bool) {
	// Stage 1: replay durable history up to the snapshot point.
	if backlogMax > fromSeq {
		events, err := m.store.ListEvents(context.Background(), sub.id, fromSeq, 0)
		if err != nil {
			slog.Warn("session replay failed", "session_id", sub.id, "error", err)
			m.finish(sub)
			return
		}
		for _, pe := range events {
			if pe.Seq > backlogMax {
				break
			}
			e, err := decodeEvent(pe.Kind, pe.Payload)
			if err != nil {
				slog.Warn("session event decode failed", "session_id", sub.id, "seq", pe.Seq, "error", err)
				continue
			}
			if !m.send(sub, WatchEvent{Seq: pe.Seq, Event: e}) {
				return
			}
		}
	}

	if terminal {
		m.finish(sub)
		return
	}

	// Stage 2: stream live events as they are enqueued.
	for {
		m.hub.mu.Lock()
		if sub.closed {
			m.hub.mu.Unlock()
			m.finish(sub)
			return
		}
		batch := sub.pending
		sub.pending = nil
		m.hub.mu.Unlock()

		if len(batch) == 0 {
			select {
			case <-sub.notify:
			case <-sub.ctx.Done():
				m.finish(sub)
				return
			case <-sub.dead:
				m.finish(sub)
				return
			}
			continue
		}

		for _, we := range batch {
			if !m.send(sub, we) {
				return
			}
			if isTerminalStateChange(we.Event) {
				m.finish(sub)
				return
			}
		}
	}
}

// send delivers one event to sub.ch, returning false (after closing the
// subscriber) if the consumer disconnected or the subscriber was lag-killed.
func (m *manager) send(sub *subscriber, we WatchEvent) bool {
	select {
	case sub.ch <- we:
		return true
	case <-sub.ctx.Done():
		m.finish(sub)
		return false
	case <-sub.dead:
		m.finish(sub)
		return false
	}
}

// finish removes the subscriber from the hub and closes its channel. It is
// called exactly once, by the feeder goroutine.
func (m *manager) finish(sub *subscriber) {
	m.hub.mu.Lock()
	sub.closed = true
	subs := m.hub.subs[sub.id]
	for i, s := range subs {
		if s == sub {
			m.hub.subs[sub.id] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	if len(m.hub.subs[sub.id]) == 0 {
		delete(m.hub.subs, sub.id)
	}
	m.hub.mu.Unlock()
	close(sub.ch)
}

func isTerminalStateChange(e Event) bool {
	sc, ok := e.(StateChangeEvent)
	return ok && sc.Session != nil && sc.Session.State.IsTerminal()
}

func (m *manager) ListEvents(ctx context.Context, id string, afterSeq int64, limit int) ([]WatchEvent, error) {
	// Surface a clear not-found error rather than an empty slice.
	if _, err := m.store.GetSession(ctx, id); err != nil {
		return nil, err
	}
	events, err := m.store.ListEvents(ctx, id, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	out := make([]WatchEvent, 0, len(events))
	for _, pe := range events {
		e, err := decodeEvent(pe.Kind, pe.Payload)
		if err != nil {
			return nil, fmt.Errorf("decode event seq %d: %w", pe.Seq, err)
		}
		out = append(out, WatchEvent{Seq: pe.Seq, Event: e})
	}
	return out, nil
}

func copySession(s *Session) *Session {
	cp := *s
	return &cp
}
