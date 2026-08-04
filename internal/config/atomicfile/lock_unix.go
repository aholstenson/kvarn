//go:build unix

package atomicfile

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// WithLock takes an exclusive advisory file lock on "<path>.lock" and runs fn
// while holding it. Wrap a load → mutate → save sequence in this to make the
// read-modify-write safe across processes: a single in-process Mutex doesn't
// help when `kvarn key create` (a separate process) races the orchestrator
// against the same file.
//
// The lock is advisory (flock(2)) — only callers that also take it are
// serialized. Reads don't need it because atomicfile.Write renames into place.
// The lock file persists at "<path>.lock" with mode 0600; it is created on
// demand alongside the data file's directory.
func WithLock(path string, fn func() error) (err error) {
	lock, err := Acquire(context.Background(), path, true)
	if err != nil {
		return err
	}
	defer lock.Release()
	return fn()
}

// Lock is a held advisory file lock. Release it exactly once, normally with a
// defer immediately after acquiring.
type Lock struct {
	f *os.File
}

// Release drops the lock.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	// Released implicitly by Close; the explicit unlock is a no-op safety net.
	syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
	l.f = nil
}

// lockPollInterval is how often a contended Acquire retries. Short enough that
// waiting costs nothing noticeable next to the operations being serialized
// (fetches, repacks), long enough not to spin.
const lockPollInterval = 20 * time.Millisecond

// Acquire takes an advisory lock on "<path>.lock", exclusive for writers and
// shared for readers, and returns it. Many shared holders coexist; an exclusive
// holder excludes everyone.
//
// It polls with LOCK_NB rather than blocking in flock(2) because a blocking
// flock cannot be interrupted: a job cancelled while queued behind a multi-gigabyte
// fetch has to be able to unwind, and a caller with a deadline has to be able to
// reach it.
func Acquire(ctx context.Context, path string, exclusive bool) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}

	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}

	for {
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		if err == nil {
			return &Lock{f: f}, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			f.Close()
			return nil, err
		}

		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(lockPollInterval):
		}
	}
}
