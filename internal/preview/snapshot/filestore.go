package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aholstenson/kvarn/internal/config/atomicfile"
)

// DirName is the directory preview state lives in under the kvarn cache root.
// It is a sibling of the tool caches, the cloned mirrors and the VM images
// rather than a child of any of them.
const DirName = "preview-state"

const (
	archiveExt = ".tar.zst"
	metaExt    = ".meta"
	// prevSuffix names the rotated generation. A ref label is one DNS label and
	// therefore cannot contain a dot, so this can never be mistaken for the
	// current archive of a preview whose ref happens to end in "prev".
	prevSuffix = ".prev"
)

// FileStore is the on-disk Store: one directory per project, one archive plus
// one sidecar plus one rotated generation per ref.
type FileStore struct {
	// BaseDir is the root the tree is built under.
	BaseDir string
	// Clock supplies the current time. Defaults to time.Now; specs override it
	// so the prune horizon can be moved without waiting.
	Clock func() time.Time
}

var _ Store = (*FileStore)(nil)

// DefaultDir is where preview state is kept when the operator names no other
// place: beside the caches, under the user's cache directory.
func DefaultDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("determine user cache dir: %w", err)
	}
	return filepath.Join(dir, "kvarn", DirName), nil
}

// NewFileStore returns a store rooted at dir.
func NewFileStore(dir string) *FileStore { return &FileStore{BaseDir: dir} }

func (f *FileStore) now() time.Time {
	if f.Clock != nil {
		return f.Clock()
	}
	return time.Now()
}

func (f *FileStore) projectDir(id ID) string {
	return filepath.Join(f.BaseDir, id.ProjectID)
}

func (f *FileStore) archivePath(id ID) string {
	return filepath.Join(f.projectDir(id), id.RefLabel+archiveExt)
}

func (f *FileStore) prevPath(id ID) string {
	return filepath.Join(f.projectDir(id), id.RefLabel+prevSuffix+archiveExt)
}

func (f *FileStore) metaPath(id ID) string {
	return filepath.Join(f.projectDir(id), id.RefLabel+metaExt)
}

// validate rejects an ID that could not stand as a path component. Both halves
// are produced by code (cache.ProjectID, project.RefLabel) rather than typed by
// a person, so this is a guard against a caller wiring something else in, not a
// user-facing check.
func (f *FileStore) validate(id ID) error {
	for _, part := range []string{id.ProjectID, id.RefLabel} {
		if part == "" {
			return errors.New("snapshot id has an empty component")
		}
		if strings.ContainsAny(part, `/\`) || part == "." || part == ".." {
			return fmt.Errorf("snapshot id component %q is not a single path element", part)
		}
	}
	return nil
}

func (f *FileStore) Open(id ID) (io.ReadCloser, Meta, error) {
	if err := f.validate(id); err != nil {
		return nil, Meta{}, err
	}
	file, err := os.Open(f.archivePath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, Meta{}, ErrNoSnapshot
	}
	if err != nil {
		return nil, Meta{}, fmt.Errorf("open preview state archive: %w", err)
	}
	// The archive is the truth about whether there is state; a sidecar that is
	// missing or unreadable costs the caller some description of it, not the
	// data itself.
	meta, _ := f.readMeta(id)
	return file, meta, nil
}

func (f *FileStore) Stat(id ID) (Meta, error) {
	if err := f.validate(id); err != nil {
		return Meta{}, err
	}
	info, err := os.Stat(f.archivePath(id))
	if errors.Is(err, os.ErrNotExist) {
		return Meta{}, ErrNoSnapshot
	}
	if err != nil {
		return Meta{}, fmt.Errorf("stat preview state archive: %w", err)
	}
	meta, err := f.readMeta(id)
	if err != nil {
		// Fall back to what the file itself says, so an archive with a damaged
		// sidecar still reports a size and an age.
		meta = Meta{CreatedAt: info.ModTime().UTC(), Bytes: info.Size()}
	}
	return meta, nil
}

func (f *FileStore) Save(id ID, meta Meta, r io.Reader) error {
	if err := f.validate(id); err != nil {
		return err
	}
	dir := f.projectDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create preview state dir %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file for preview state: %w", err)
	}
	tmpName := tmp.Name()
	n, copyErr := io.Copy(tmp, r)
	if copyErr != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write preview state archive: %w", copyErr)
	}
	// The archive is the only copy of what somebody entered into the preview, so
	// it reaches the disk before the rename makes it the current one.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("flush preview state archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close preview state archive: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("chmod preview state archive: %w", err)
	}

	// Keep one generation. A capture that failed halfway through — a tar that
	// stopped mid-stream, a host that ran out of disk — must not be the reason
	// the only copy is gone.
	dest := f.archivePath(id)
	if _, err := os.Stat(dest); err == nil {
		if err := os.Rename(dest, f.prevPath(id)); err != nil {
			os.Remove(tmpName)
			return fmt.Errorf("rotate previous preview state archive: %w", err)
		}
	}
	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("commit preview state archive: %w", err)
	}

	now := f.now().UTC()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	meta.Bytes = n
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encode preview state metadata: %w", err)
	}
	if err := atomicfile.Write(f.metaPath(id), data, 0o600); err != nil {
		return fmt.Errorf("write preview state metadata: %w", err)
	}
	// Age is the archive's mtime, so the file it describes carries the same
	// stamp the sidecar records rather than whatever the copy happened to take.
	_ = os.Chtimes(dest, now, now)
	return nil
}

func (f *FileStore) Touch(id ID) error {
	if err := f.validate(id); err != nil {
		return err
	}
	now := f.now().UTC()
	err := os.Chtimes(f.archivePath(id), now, now)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNoSnapshot
	}
	if err != nil {
		return fmt.Errorf("touch preview state archive: %w", err)
	}
	return nil
}

func (f *FileStore) Delete(id ID) error {
	if err := f.validate(id); err != nil {
		return err
	}
	for _, path := range []string{f.archivePath(id), f.prevPath(id), f.metaPath(id)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("delete preview state: %w", err)
		}
	}
	// An empty project directory is noise in a tree an operator is expected to
	// be able to read.
	_ = os.Remove(f.projectDir(id))
	return nil
}

func (f *FileStore) Prune(olderThan time.Duration, keep func(ID) bool) (PruneReport, error) {
	var report PruneReport
	// A zero or negative retention is the operator saying "never prune", not
	// "prune everything" — the sweep would otherwise delete the whole store on
	// a misconfigured value.
	if olderThan <= 0 {
		return report, nil
	}

	horizon := f.now().Add(-olderThan)
	ids, err := f.list()
	if errors.Is(err, os.ErrNotExist) {
		return report, nil
	}
	if err != nil {
		return report, err
	}

	for _, id := range ids {
		if keep != nil && keep(id) {
			continue
		}
		info, err := os.Stat(f.archivePath(id))
		if err != nil {
			continue
		}
		if !info.ModTime().Before(horizon) {
			continue
		}
		freed := info.Size()
		if prev, err := os.Stat(f.prevPath(id)); err == nil {
			freed += prev.Size()
		}
		if err := f.Delete(id); err != nil {
			return report, err
		}
		report.Removed++
		report.BytesFreed += freed
	}
	return report, nil
}

// List returns every archive the store holds, ordered so output is stable.
func (f *FileStore) List() ([]ID, error) {
	ids, err := f.list()
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return ids, err
}

func (f *FileStore) list() ([]ID, error) {
	projects, err := os.ReadDir(f.BaseDir)
	if err != nil {
		return nil, err
	}
	var ids []ID
	for _, proj := range projects {
		if !proj.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(f.BaseDir, proj.Name()))
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, archiveExt) {
				continue
			}
			label := strings.TrimSuffix(name, archiveExt)
			if strings.HasSuffix(label, prevSuffix) {
				// A rotated generation is not an entry of its own; it goes with
				// the archive it backs up.
				continue
			}
			ids = append(ids, ID{ProjectID: proj.Name(), RefLabel: label})
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		if ids[i].ProjectID != ids[j].ProjectID {
			return ids[i].ProjectID < ids[j].ProjectID
		}
		return ids[i].RefLabel < ids[j].RefLabel
	})
	return ids, nil
}

func (f *FileStore) readMeta(id ID) (Meta, error) {
	data, err := os.ReadFile(f.metaPath(id))
	if err != nil {
		return Meta{}, err
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return Meta{}, fmt.Errorf("parse preview state metadata: %w", err)
	}
	return m, nil
}
