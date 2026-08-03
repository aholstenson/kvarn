//go:build linux

package disk

import (
	"fmt"
	"os/exec"
	"path/filepath"
)

// CreateOverlayQcow2 creates a qcow2 image at path that reads through to
// backing for any cluster it has not written itself, with a virtual size of
// sizeBytes. The overlay starts out a few hundred kilobytes regardless of how
// large the backing image is, so a VM boots without first duplicating the base
// image; the backing file is only ever read, which is what lets concurrent VMs
// share one copy of it in the host page cache.
//
// sizeBytes is raised to the backing image's virtual size when it is smaller,
// since an overlay cannot expose less than what it reads through to.
func CreateOverlayQcow2(path, backing string, sizeBytes int64) error {
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		return fmt.Errorf("qemu-img not found: %w", err)
	}

	// The backing path is recorded in the overlay header and resolved by QEMU
	// relative to the overlay, which lives in a different directory.
	backingAbs, err := filepath.Abs(backing)
	if err != nil {
		return fmt.Errorf("resolve backing path: %w", err)
	}

	backingSize, err := Qcow2VirtualSize(backingAbs)
	if err != nil {
		return err
	}
	if sizeBytes < backingSize {
		sizeBytes = backingSize
	}

	// -F declares the backing format explicitly; without it QEMU probes the
	// backing file on every open and warns.
	cmd := exec.Command(qemuImg, "create",
		"-f", "qcow2",
		"-F", "qcow2",
		"-b", backingAbs,
		path,
		fmt.Sprintf("%d", sizeBytes),
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img create: %s: %w", output, err)
	}

	return nil
}
