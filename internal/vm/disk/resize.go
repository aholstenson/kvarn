package disk

import (
	"os"

	"github.com/cockroachdb/errors"
)

// ResizeDisk extends the disk image file to sizeBytes. The partition table
// and ext4 filesystem are expanded in the guest by cloud-init (growpart +
// resizefs modules).
func ResizeDisk(path string, sizeBytes int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return errors.Wrap(err, "stat disk image")
	}

	if sizeBytes <= info.Size() {
		return nil // already large enough
	}

	if err := os.Truncate(path, sizeBytes); err != nil {
		return errors.Wrap(err, "truncate disk image")
	}

	return nil
}
