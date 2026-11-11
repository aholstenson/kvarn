package sandbox

import (
	"context"
	"time"

	"github.com/aholstenson/kvarn/internal/dispatch"
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

// SetBootRetryForTest replaces the boot retry schedule and the runner
// registration timeout so specs can exercise boot retries without waiting out
// the real values. It returns a function that restores the previous values.
func SetBootRetryForTest(waits []time.Duration, registerTimeout time.Duration) func() {
	prevWaits, prevTimeout := bootRetryBackoff, runnerRegisterTimeout
	bootRetryBackoff, runnerRegisterTimeout = waits, registerTimeout
	return func() { bootRetryBackoff, runnerRegisterTimeout = prevWaits, prevTimeout }
}

// BootVMForTest runs the boot step of Start, retries included, against
// opts.Provider. It returns the ID of the VM that booted and a function that
// releases everything the boot acquired.
func BootVMForTest(ctx context.Context, opts Opts, registry *dispatch.Registry) (string, func(), error) {
	b, err := bootVM(ctx, opts, opts.CreateOpts, registry, dispatch.NewHandler(registry))
	if err != nil {
		return "", nil, err
	}
	return b.instance.ID, b.release, nil
}
