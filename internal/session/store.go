package session

import (
	"context"
	"errors"
	"time"
)

// ErrIdempotencyConflict is returned by CreateSession when the session's
// idempotency key is already held by another session in the same project. The
// caller resolves it by reading that session back and returning it, which is
// how the loser of a race between two copies of one retried request still ends
// up describing the single job that was created.
var ErrIdempotencyConflict = errors.New("idempotency key already used")

// PersistedEvent is one durable entry in a session's monotonic event log.
type PersistedEvent struct {
	SessionID  string
	Seq        int64 // per-session monotonic, starts at 1
	Kind       string
	Payload    []byte // JSON, see codec.go; truncated at payloadCap
	RecordedAt time.Time
}

// SessionFilter constrains a ListSessions query. The zero value matches every
// session with no limit; the fields are ANDed.
type SessionFilter struct {
	Project    string  // "" = any
	PRRef      string  // "" = any; exact match on the session's pull request ref
	Mode       string  // "" = any
	States     []State // nil = any; exact match on the session's state
	ActiveOnly bool    // non-terminal only, applied on top of States
	// CreatedAfter bounds the listing to sessions created strictly after it.
	// It is deliberately not part of the pagination cursor: that orders by
	// (created_at DESC, id DESC) and walks backwards, while this is a floor the
	// whole listing is held above.
	CreatedAfter time.Time
	// Metadata pairs the session must carry, all of them, each matched exactly.
	// An empty map matches every session; a key present with an empty value
	// matches only a session that stored that key with an empty value.
	Metadata map[string]string
	Limit    int // 0 = no limit
	// AfterCreatedAt / AfterID form a cursor for keyset pagination, ordered by
	// (created_at DESC, id DESC). A zero AfterCreatedAt starts from the top.
	AfterCreatedAt time.Time
	AfterID        string
}

// PendingQuery selects and orders the backlog for dispatch.
//
// Entries are ranked by effective priority — the configured priority plus one
// level per AgeStep waited — highest first, then by arrival. That is the same
// rule the admission queue's Fair policy applies once a job is in memory,
// including its clamp: aging can lift an entry no higher than the highest
// priority currently in the backlog, so it closes a gap an operator opened and
// never invents one. Without it a backlog where nobody set a priority would
// order purely by age bucket, which is a coarser FIFO than just using arrival.
//
// A zero AgeStep disables aging and lets priority strictly dominate.
type PendingQuery struct {
	Now     time.Time
	AgeStep time.Duration
	Limit   int
}

// EffectivePriority returns the priority the backlog actually orders s by:
// its configured priority plus one level per AgeStep waited, clamped to
// ceiling. It is the Go statement of the rule the SQLite ORDER BY expresses,
// and exists so a caller that has to *report* an entry's place — rather than
// merely receive rows in order — reads it from the same definition.
func (q PendingQuery) EffectivePriority(s *Session, ceiling int) int {
	p := s.Priority
	if q.AgeStep <= 0 {
		return p
	}
	p += int(q.Now.Sub(s.QueuedAt) / q.AgeStep)
	return min(p, ceiling)
}

// PendingCeiling is the clamp EffectivePriority applies: the highest configured
// priority among the given backlog entries. Zero for an empty backlog.
func PendingCeiling(sessions []*Session) int {
	ceiling := 0
	for i, s := range sessions {
		if i == 0 || s.Priority > ceiling {
			ceiling = s.Priority
		}
	}
	return ceiling
}

// PendingTransition is the target of a TransitionPending call: the state to
// move to plus the message and error to record with it.
type PendingTransition struct {
	State   State
	Message string
	Error   string
}

// RequeueOpts is the target of a RequeueRun call: a live run's return to the
// backlog, made by a host that has decided to stop starting work rather than by
// the run failing.
type RequeueOpts struct {
	// Message is recorded on the session, so the entry says why it is waiting
	// again rather than looking like a fresh submission.
	Message string
	// MaxAttempts caps how many dispatches a job may spend, as in ReconcileOpts.
	// A job already at the cap is not requeued: the caller is stopping it either
	// way, and putting it back only to have the next boot fail it is worse than
	// saying so now. Zero means no cap.
	MaxAttempts int
}

// ReconcileOpts configures startup reconciliation.
type ReconcileOpts struct {
	// MaxAttempts caps how many times one job may be dispatched. A restartable
	// session that has already used them up fails instead of returning to the
	// backlog, so a job that kills the orchestrator on every attempt stops
	// killing it. Zero means no cap.
	MaxAttempts int
	// RequeueMessage is recorded on sessions returned to the backlog, and
	// FailError on those failed outright.
	RequeueMessage string
	FailError      string
}

// ReconcileResult reports what startup reconciliation did, split by outcome so
// the caller can log a requeue (routine) differently from a failure (a job
// somebody was waiting on that will not come back).
type ReconcileResult struct {
	Requeued []string
	Failed   []string
}

// Store is the pure-persistence layer beneath the session Manager: durable
// session records plus a per-session monotonic event log. Implementations need
// not provide pub/sub — the Manager owns the live hub and layers replay on top.
type Store interface {
	// CreateSession persists a new session. It returns ErrIdempotencyConflict
	// when the session carries an idempotency key another session in the same
	// project already holds, so two concurrent submissions of one retried
	// request settle in the store rather than both creating a job.
	CreateSession(ctx context.Context, s *Session) error
	GetSession(ctx context.Context, id string) (*Session, error)
	// FindByIdempotencyKey returns the session holding key within project, or
	// nil when none does. An empty key matches nothing: a submission without a
	// key claims nothing and must never be handed back as somebody's retry.
	FindByIdempotencyKey(ctx context.Context, project, key string) (*Session, error)
	// UpdateSession persists the mutable fields of s (state, message, error,
	// pull_request_url, pr_ref, head_branch, cost, updated_at). It never
	// touches the event log.
	UpdateSession(ctx context.Context, s *Session) error
	ListSessions(ctx context.Context, filter SessionFilter) ([]*Session, error)

	// AppendEvent assigns the next per-session seq (starting at 1) and persists
	// the event atomically, returning the stored record.
	AppendEvent(ctx context.Context, sessionID, kind string, payload []byte) (PersistedEvent, error)
	// ListEvents returns events with seq > afterSeq in ascending seq order,
	// capped at limit (0 = no limit).
	ListEvents(ctx context.Context, sessionID string, afterSeq int64, limit int) ([]PersistedEvent, error)
	// MaxSeq returns the highest seq recorded for the session, or 0 if none.
	MaxSeq(ctx context.Context, sessionID string) (int64, error)

	// ListPending returns backlog entries in dispatch order, capped at limit
	// (0 = no limit). See PendingQuery for the ordering.
	ListPending(ctx context.Context, q PendingQuery) ([]*Session, error)
	// CountPending returns the number of sessions in the backlog.
	CountPending(ctx context.Context) (int, error)
	// TransitionPending atomically moves a session out of StatePending,
	// appending the resulting state_change event in the same transaction. It
	// reports false without an error when the session is no longer pending,
	// which is how the dispatcher and a concurrent cancel settle their race:
	// both attempt the move and exactly one of them wins.
	TransitionPending(ctx context.Context, id string, to PendingTransition) (bool, PersistedEvent, error)
	// UpdatePendingPriority reorders a backlog entry, returning the priority it
	// replaced. It reports false without an error when the session is no longer
	// pending: past dispatch the value orders nothing, since the admission
	// queue already holds the request built from it, so silently rewriting the
	// column would promise a reordering that never happens.
	UpdatePendingPriority(ctx context.Context, id string, priority int) (previous int, ok bool, err error)
	// RequeueRun returns a live run to the backlog, atomically and only from a
	// restartable state, incrementing its attempt count and appending the
	// resulting state_change event. It reports false without an error when the
	// session has moved on — a run that reached StateRunning while it was being
	// signalled has spent budget and holds agent work, so it is no longer
	// something a drain may silently re-run. That check has to live in the same
	// transaction as the write, which is why this is a store operation rather
	// than a read followed by UpdateState.
	RequeueRun(ctx context.Context, id string, opts RequeueOpts) (bool, PersistedEvent, error)
	// ExpirePending fails backlog entries queued before cutoff. A separate
	// sweep rather than a filter on ListPending because a low-priority entry
	// may never reach the head of the dispatch order, and an entry nobody looks
	// at is exactly the one that goes stale.
	ExpirePending(ctx context.Context, cutoff time.Time, reason string) ([]string, error)

	// ReconcileStartup settles every session a dead orchestrator left behind.
	// Restartable states return to the backlog with their attempt count
	// incremented; everything else non-terminal fails. Pending sessions are
	// left alone — they never left the backlog. Called once at startup, since
	// the VMs these sessions referenced are gone either way.
	ReconcileStartup(ctx context.Context, opts ReconcileOpts) (ReconcileResult, error)
	// PruneTerminalBefore deletes terminal sessions whose created_at is before
	// cutoff; their events cascade. Returns the number of sessions removed.
	PruneTerminalBefore(ctx context.Context, cutoff time.Time) (int, error)

	Close() error
}
