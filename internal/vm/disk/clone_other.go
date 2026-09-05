//go:build !darwin

package disk

import "errors"

// CloneFile is macOS-only: it needs APFS copy-on-write clones. Linux gives each
// VM a qcow2 overlay instead, which reads through to the shared base image.
func CloneFile(_, _ string) error {
	return errors.ErrUnsupported
}
