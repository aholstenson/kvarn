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
	ActiveOnly bool   // non-terminal only
	Limit      int    // 0 = no limit
	// AfterCreatedAt / AfterID form a cursor for keyset pagination, ordered by
	// (created_at DESC, id DESC). A zero AfterCreatedAt starts from the top.
	AfterCreatedAt time.Time
	AfterID        string
}

// Store is the pure-persistence layer beneath the session Manager: durable
// session records plus a per-session monotonic event log. Implementations need
// not provide pub/sub — the Manager owns the live hub and layers replay on top.
type Store interface {
	CreateSession(ctx context.Context, s *Session) error
	GetSession(ctx context.Context, id string) (*Session, error)
	// UpdateSession persists the mutable fields of s (state, message, error,
	// pull_request_url, cost, updated_at). It never touches the event log.
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

	// ReconcileNonTerminal flips every non-terminal session to failed with the
	// given reason, appending a state_change event to each. Returns the ids
	// affected. Called once at startup since the VMs they referenced are gone.
	ReconcileNonTerminal(ctx context.Context, reason string) ([]string, error)
	// PruneTerminalBefore deletes terminal sessions whose created_at is before
	// cutoff; their events cascade. Returns the number of sessions removed.
	PruneTerminalBefore(ctx context.Context, cutoff time.Time) (int, error)

	Close() error
}
