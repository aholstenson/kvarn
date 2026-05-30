//go:build unix

package atomicfile

import (
	"os"
	"path/filepath"
	"syscall"
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	// Released implicitly by f.Close; explicit unlock is a no-op safety net.
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}
