package disk

import (
	"fmt"
	"os"
)

// ResizeDisk extends the disk image file to sizeBytes. The partition table
// and ext4 filesystem are expanded in the guest by cloud-init (growpart +
// resizefs modules).
func ResizeDisk(path string, sizeBytes int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat disk image: %w", err)
	}

	if sizeBytes <= info.Size() {
		return nil // already large enough
	}

	if err := os.Truncate(path, sizeBytes); err != nil {
		return fmt.Errorf("truncate disk image: %w", err)
	}

	return nil
}
