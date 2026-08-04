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

// hub.mu guards the subscriber map and every field marked as such on
// subscriber. It is only ever held across in-memory work — never across a Store
// call — so one session's durable append cannot stall another session's live
// broadcast. Ordering against the store is the sequencing lock's job.
type hub struct {
	mu   sync.Mutex
	subs map[string][]*subscriber
}

// seqStripes is the number of mutexes that serialize sequencing. A session
// hashes to one stripe, so the ordering guarantee below holds per session while
// unrelated sessions proceed in parallel. Two sessions sharing a stripe
// serialize against each other, which costs a little false contention and never
// correctness; the count is sized well above the concurrent-job ceiling so
// collisions stay rare.
const seqStripes = 64

// manager owns the in-memory pub/sub hub and delegates all persistence to a
// Store, layering replay + reconnect-from-cursor on top of the live stream.
type manager struct {
	store Store
	hub   hub
	seq   [seqStripes]sync.Mutex
}

// seqLock returns the mutex serializing sequencing for a session. Holding it
// across "assign a seq, then enqueue to subscribers" is what makes the order
// events reach watchers equal the order the store numbered them, and what stops
// a registering Watch from snapshotting MaxSeq in the middle of an append and
// missing the event on both the replay and the live side.
func (m *manager) seqLock(id string) *sync.Mutex {
	// FNV-1a, inlined to keep the hot path allocation-free. Session ids are
	// random hex, so any cheap hash spreads them evenly.
	h := uint32(2166136261)
	for i := 0; i < len(id); i++ {
		h ^= uint32(id[i])
		h *= 16777619
	}
	return &m.seq[h%seqStripes]
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

func (m *manager) Create(ctx context.Context, params CreateParams) (*Session, error) {
	id, err := generateID()
	if err != nil {
		return nil, fmt.Errorf("generate session id: %w", err)
	}
	now := time.Now()
	s := &Session{
		ID:              id,
		ProjectName:     params.ProjectName,
		Prompt:          params.Prompt,
		Mode:            params.Mode,
		State:           StatePending,
		PRRef:           params.PRRef,
		HeadBranch:      params.HeadBranch,
		BaseBranch:      params.BaseBranch,
		ParentSessionID: params.ParentSessionID,
		KeyID:           params.KeyID,
		Priority:        params.Priority,
		CreatedAt:       now,
		UpdatedAt:       now,
		// A new session enters the backlog at creation, so its queue age starts
		// with it. A requeue moves this forward; CreatedAt never moves.
		QueuedAt: now,
	}
	if err := m.store.CreateSession(ctx, s); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return copySession(s), nil
}

func (m *manager) Get(ctx context.Context, id string) (*Session, error) {
	return m.store.GetSession(ctx, id)
}

func (m *manager) ListPending(ctx context.Context, q PendingQuery) ([]*Session, error) {
	return m.store.ListPending(ctx, q)
}

func (m *manager) CountPending(ctx context.Context) (int, error) {
	return m.store.CountPending(ctx)
}

// UpdatePendingPriority reorders a backlog entry. No event is emitted: nothing
// observable about the session changed, only where it sits among the others
// waiting, which a watcher of this session cannot see anyway.
func (m *manager) UpdatePendingPriority(ctx context.Context, id string, priority int) (int, bool, error) {
	return m.store.UpdatePendingPriority(ctx, id, priority)
}

// TransitionPending moves a session out of the backlog and, when it wins the
// move, broadcasts the state change the store persisted with it.
//
// The store numbered that event inside its own transaction, so unlike
// persistAndBroadcast this only has to deliver it. Doing so outside the store's
// transaction would reorder against a concurrent append on the same session —
// but a pending session has no other event source, since nothing runs against
// it until precisely this call hands it to one.
func (m *manager) TransitionPending(ctx context.Context, id string, to PendingTransition) (bool, error) {
	ok, pe, err := m.store.TransitionPending(ctx, id, to)
	if err != nil || !ok {
		return false, err
	}
	event, err := decodeEvent(pe.Kind, pe.Payload)
	if err != nil {
		// The transition is committed; failing here would tell the caller it
		// did not happen. Watchers still converge — they poll or reconnect
		// against the store, which has the event.
		slog.Warn("could not broadcast pending transition", "session_id", id, "error", err)
		return true, nil
	}
	seqLock := m.seqLock(id)
	seqLock.Lock()
	defer seqLock.Unlock()
	m.hub.mu.Lock()
	defer m.hub.mu.Unlock()
	m.broadcastLocked(id, WatchEvent{Seq: pe.Seq, Event: event})
	return true, nil
}

func (m *manager) RequeueRun(ctx context.Context, id string, opts RequeueOpts) (bool, error) {
	ok, pe, err := m.store.RequeueRun(ctx, id, opts)
	if err != nil || !ok {
		return false, err
	}
	event, err := decodeEvent(pe.Kind, pe.Payload)
	if err != nil {
		// The requeue is committed; reporting failure here would tell the
		// caller the run is still theirs to finish. Watchers converge anyway,
		// against the store that holds the event.
		slog.Warn("could not broadcast requeue", "session_id", id, "error", err)
		return true, nil
	}
	seqLock := m.seqLock(id)
	seqLock.Lock()
	defer seqLock.Unlock()
	m.hub.mu.Lock()
	defer m.hub.mu.Unlock()
	m.broadcastLocked(id, WatchEvent{Seq: pe.Seq, Event: event})
	return true, nil
}

func (m *manager) ExpirePending(ctx context.Context, cutoff time.Time, reason string) ([]string, error) {
	return m.store.ExpirePending(ctx, cutoff, reason)
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

func (m *manager) SetPullRequest(ctx context.Context, id, url, ref, branch string) error {
	s, err := m.store.GetSession(ctx, id)
	if err != nil {
		return err
	}
	s.PullRequestURL = url
	s.PRRef = ref
	s.HeadBranch = branch
	s.UpdatedAt = time.Now()
	if err := m.store.UpdateSession(ctx, s); err != nil {
		return err
	}
	return m.persistAndBroadcast(ctx, id, PullRequestEvent{
		SessionID: id,
		URL:       url,
		Ref:       ref,
		Branch:    branch,
	})
}

// EmitEvent carries no existence check of its own. The high-volume kinds —
// console output, step stdout/stderr, transfer/cache/dependency progress — are
// live-only, and a pre-read would put a Store round trip on the one path that
// otherwise touches no storage at all. Durable kinds still fail loudly on an
// unknown session: AppendEvent rejects them, in SQLite through the
// session_events → sessions foreign key.
func (m *manager) EmitEvent(ctx context.Context, id string, event Event) error {
	return m.persistAndBroadcast(ctx, id, event)
}

// persistAndBroadcast persists durable events and enqueues every event to live
// subscribers, holding the session's seqLock across both so the seq the store
// assigns equals the seq broadcast, in order. Ephemeral events take the same
// lock: they carry seq 0 and so have nothing to order among themselves, but a
// console line that overtook the agent message it followed would still be a
// visible reordering to a watcher.
func (m *manager) persistAndBroadcast(ctx context.Context, id string, e Event) error {
	kind, payload, durable, err := encodeEvent(e)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}

	seqLock := m.seqLock(id)
	seqLock.Lock()
	defer seqLock.Unlock()

	seq := int64(0)
	if durable {
		pe, err := m.store.AppendEvent(ctx, id, kind, payload)
		if err != nil {
			return fmt.Errorf("append event: %w", err)
		}
		seq = pe.Seq
	}

	m.hub.mu.Lock()
	defer m.hub.mu.Unlock()
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

	// Snapshot the backlog and register under the seqLock so no append can land
	// between the two: such an event would be past the replay cutoff and absent
	// from the live stream, a gap the client has no way to detect.
	seqLock := m.seqLock(id)
	seqLock.Lock()
	backlogMax, err := m.store.MaxSeq(ctx, id)
	if err != nil {
		seqLock.Unlock()
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
		m.hub.mu.Lock()
		m.hub.subs[id] = append(m.hub.subs[id], sub)
		m.hub.mu.Unlock()
	}
	seqLock.Unlock()

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
