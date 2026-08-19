package sandbox

import (
	"context"
	"time"

	"github.com/aholstenson/kvarn/internal/project"
)

// PinNixpkgsRevsForTest exposes the channel-to-commit rewrite Start applies to
// resolved dependencies.
func PinNixpkgsRevsForTest(ctx context.Context, deps []project.ResolvedDep, revOf NixpkgsRevFunc) []project.ResolvedDep {
	return pinNixpkgsRevs(ctx, deps, revOf)
}

// SetDependencyRetryBackoffForTest replaces the dependency install retry
// schedule so specs can exercise the retry path without waiting out the real
// backoff. It returns a function that restores the previous schedule.
func SetDependencyRetryBackoffForTest(waits []time.Duration) func() {
	prev := dependencyRetryBackoff
	dependencyRetryBackoff = waits
	return func() { dependencyRetryBackoff = prev }
}

// NewTestSession returns an empty Session for use in unit tests.
func NewTestSession() *Session {
	return &Session{}
}

// AddCloserForTest registers a closer function on the session, mirroring the
// internal addCloser method so tests can exercise Close() without a real VM.
func (s *Session) AddCloserForTest(fn func()) {
	s.addCloser(fn)
}
