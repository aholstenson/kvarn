//go:build !unix

package atomicfile

import (
	"context"
	"errors"
)

// WithLock is unimplemented on non-unix platforms; kvarn currently targets
// linux and darwin only.
func WithLock(path string, fn func() error) error {
	return errors.ErrUnsupported
}

// Lock is the non-unix placeholder for a held advisory file lock.
type Lock struct{}

// Release is a no-op; no lock is ever acquired on this platform.
func (l *Lock) Release() {}

// Acquire is unimplemented on non-unix platforms.
func Acquire(ctx context.Context, path string, exclusive bool) (*Lock, error) {
	return nil, errors.ErrUnsupported
}
