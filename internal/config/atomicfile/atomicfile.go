// Package atomicfile writes files atomically so a concurrent reader never
// observes a partially written file. The config stores re-read their backing
// file on every operation, so a CLI write racing with an in-flight read must
// appear all-or-nothing.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write writes data to path with the given mode by creating a temp file in the
// same directory, applying the mode, and renaming it into place. The rename is
// atomic on POSIX filesystems, so readers see either the old or the new file.
func Write(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we fail before the rename; a no-op once renamed.
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
