//go:build darwin

package disk

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// CloneFile makes dst a copy-on-write clone of src. On APFS the two files share
// their blocks until one of them is written, so a multi-gigabyte disk image is
// copied in milliseconds and costs space only for what a VM writes.
//
// dst must not exist yet — the clone creates it. Both paths must be on the same
// APFS volume; anywhere else the call fails and the caller must copy instead.
//
// The clone is independent of its source once made, so the source can be
// replaced or deleted while a VM runs from the clone.
func CloneFile(src, dst string) error {
	if err := unix.Clonefile(src, dst, 0); err != nil {
		return fmt.Errorf("clone %s to %s: %w", src, dst, err)
	}
	return nil
}
