//go:build linux

package disk

import (
	"fmt"
	"os/exec"

	"github.com/cockroachdb/errors"
)

// ResizeQcow2 resizes a qcow2 disk image to sizeBytes using qemu-img.
// QEMU handles qcow2 natively, so no format conversion is needed.
func ResizeQcow2(path string, sizeBytes int64) error {
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		return errors.Wrap(err, "qemu-img not found")
	}

	cmd := exec.Command(qemuImg, "resize", path, fmt.Sprintf("%d", sizeBytes))
	if output, err := cmd.CombinedOutput(); err != nil {
		return errors.Wrapf(err, "qemu-img resize: %s", output)
	}

	return nil
}
