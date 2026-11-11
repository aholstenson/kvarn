package session

import "time"

// Delivery is the durable record of a run's output on its way to the forge:
// the branch to push, the pull request to open, the comments to post, and
// which of those have already landed.
//
// It exists so that a delivery can outlive both a forge outage and the
// process that started it. A session with a Delivery is still owned by the
// orchestrator after the run that produced it has ended, and startup
// reconciliation leaves it alone so the delivery can resume.
type Delivery struct {
	SessionID string
	// Plan is the delivery itself, as JSON. It is opaque here: the
	// orchestrator owns the vocabulary, and the store's job is to keep it
	// until the delivery has finished.
	Plan []byte
	// Attempts counts the attempts made so far.
	Attempts int
	// NextAttemptAt is when the next attempt is due.
	NextAttemptAt time.Time
	// Deadline is when the delivery stops retrying and fails.
	Deadline time.Time
	// LastError is the error of the most recent failed attempt, or empty.
	LastError string
	CreatedAt time.Time
	UpdatedAt time.Time
}
