//go:build !unix

package atomicfile

import "errors"

// WithLock is unimplemented on non-unix platforms; kvarn currently targets
// linux and darwin only.
func WithLock(path string, fn func() error) error {
	return errors.ErrUnsupported
}
