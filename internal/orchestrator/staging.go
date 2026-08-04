package orchestrator

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/aholstenson/kvarn/internal/logging"
	"github.com/aholstenson/kvarn/internal/session"
)

// staging bounds how many jobs may clone and read their kvarn.yml at once.
//
// This work happens *before* admission and so is invisible to the capacity
// pool, but it is not free: a job's footprint comes from the repository's
// kvarn.yml, which cannot be read until the clone exists, so every queued job
// is already holding a clone on the same filesystem the disk pool is sized
// from. Without a bound here, a deep queue means that many simultaneous clones
// competing for the disk the running VMs need.
//
// A permit is held only for the clone, never across the wait for capacity —
// holding it there would make the admission queue's depth the real limit on
// concurrent clones and defeat the point.
type staging struct {
	slots chan struct{}
}

// newStaging returns a staging gate of n permits, or nil when n <= 0. A nil
// gate admits everything, so callers need no special case.
func newStaging(n int) *staging {
	if n <= 0 {
		return nil
	}
	return &staging{slots: make(chan struct{}, n)}
}

// acquire takes a permit, blocking until one is free or ctx is done. The
// returned release is idempotent, so a caller can hand the permit back as soon
// as it is done and still defer the same func as a safety net against an early
// return in between.
func (s *staging) acquire(ctx context.Context) (release func(), err error) {
	if s == nil {
		return func() {}, nil
	}
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return sync.OnceFunc(func() { <-s.slots }), nil
}

// waiting reports whether a permit would block, so a caller can tell the
// session why it is not cloning yet instead of leaving it apparently idle.
func (s *staging) waiting() bool {
	if s == nil {
		return false
	}
	return len(s.slots) == cap(s.slots)
}

// enterStaging takes a clone permit for the job, reporting the wait on the
// session when there is one. The returned release is always safe to call.
func (s *Service) enterStaging(ctx context.Context, sessionID string, log *slog.Logger) (func(), error) {
	if !s.staging.waiting() {
		return s.staging.acquire(ctx)
	}

	s.sessionMgr.UpdateState(ctx, sessionID, session.StateQueued,
		"Waiting to clone; the host is at its concurrent-clone limit")
	start := time.Now()
	release, err := s.staging.acquire(ctx)
	if err != nil {
		return nil, err
	}
	log.Info("clone slot acquired", "duration", logging.Elapsed(start))
	return release, nil
}
