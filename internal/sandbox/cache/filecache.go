package cache

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cockroachdb/errors"
)

// FileCache stores caches as tarballs under BaseDir/<projectID>/.
type FileCache struct {
	BaseDir string
}

// DefaultFileCache returns a FileCache rooted at the user's cache directory
// under a "kvarn" subdirectory (e.g. ~/.cache/kvarn on Linux/macOS).
func DefaultFileCache() (*FileCache, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return nil, errors.Wrap(err, "determine user cache dir")
	}
	return &FileCache{BaseDir: filepath.Join(dir, "kvarn")}, nil
}

func (f *FileCache) Restore(projectID string, guestPath string) (io.ReadCloser, error) {
	p := f.tarballPath(projectID, guestPath)
	file, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.Wrapf(err, "open cache tarball %s", p)
	}
	return file, nil
}

func (f *FileCache) Save(projectID string, guestPath string, data io.Reader) error {
	dir := filepath.Join(f.BaseDir, projectID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errors.Wrapf(err, "create cache dir %s", dir)
	}

	// Write a small metadata file so humans can identify which project this
	// cache belongs to.
	infoPath := filepath.Join(dir, "SOURCE")
	_ = os.WriteFile(infoPath, []byte(projectID+"\n"), 0o644)

	// Atomic write via temp file + rename.
	dest := f.tarballPath(projectID, guestPath)
	tmp, err := os.CreateTemp(dir, ".cache-*.tmp")
	if err != nil {
		return errors.Wrap(err, "create temp file for cache")
	}
	tmpName := tmp.Name()

	if _, err := io.Copy(tmp, data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return errors.Wrap(err, "write cache tarball")
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return errors.Wrap(err, "close cache tarball")
	}
	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return errors.Wrap(err, "rename cache tarball")
	}
	return nil
}

func (f *FileCache) Clear(projectID string) error {
	dir := filepath.Join(f.BaseDir, projectID)
	return os.RemoveAll(dir)
}

func (f *FileCache) tarballPath(projectID string, guestPath string) string {
	return filepath.Join(f.BaseDir, projectID, flattenPath(guestPath)+".tar.zst")
}

// flattenPath converts an absolute path into a flat filename by replacing
// slashes with underscores.
// e.g. "/home/kvarn/go/pkg/mod" → "_home_kvarn_go_pkg_mod"
func flattenPath(p string) string {
	return strings.ReplaceAll(p, "/", "_")
}
