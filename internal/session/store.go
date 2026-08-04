package session

import (
	"context"
	"time"
)

// PersistedEvent is one durable entry in a session's monotonic event log.
type PersistedEvent struct {
	SessionID  string
	Seq        int64 // per-session monotonic, starts at 1
	Kind       string
	Payload    []byte // JSON, see codec.go; truncated at payloadCap
	RecordedAt time.Time
}

// SessionFilter constrains a ListSessions query. The zero value matches every
// session with no limit.
type SessionFilter struct {
	Project    string // "" = any
	PRRef      string // "" = any; exact match on the session's pull request ref
	ActiveOnly bool   // non-terminal only
	Limit      int    // 0 = no limit
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

// PendingTransition is the target of a TransitionPending call: the state to
// move to plus the message and error to record with it.
type PendingTransition struct {
	State   State
	Message string
	Error   string
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
	CreateSession(ctx context.Context, s *Session) error
	GetSession(ctx context.Context, id string) (*Session, error)
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
