package cache

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// sharedNamespaceDir is the on-disk directory name for the empty (shared)
// namespace, since "" cannot be a path component.
const sharedNamespaceDir = "_shared"

// FileCache stores caches as content-addressed, write-once tarballs under
// BaseDir/<projectID>/<namespace>/<bucket>/<inputKey>.tar.zst, each paired
// with an <inputKey>.meta sidecar and a per-bucket LATEST MRU pointer.
type FileCache struct {
	BaseDir string

	// Clock, when set, supplies the current time for metadata timestamps.
	// Defaults to time.Now; tests override it for deterministic ordering.
	Clock func() time.Time
}

// DefaultFileCache returns a FileCache rooted at the user's cache directory
// under a "kvarn" subdirectory (e.g. ~/.cache/kvarn on Linux/macOS).
func DefaultFileCache() (*FileCache, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("determine user cache dir: %w", err)
	}
	return &FileCache{BaseDir: filepath.Join(dir, "kvarn")}, nil
}

var _ Provider = (*FileCache)(nil)

func (f *FileCache) now() time.Time {
	if f.Clock != nil {
		return f.Clock()
	}
	return time.Now()
}

func namespaceDir(ns string) string {
	if ns == "" {
		return sharedNamespaceDir
	}
	return ns
}

func (f *FileCache) projectDir(projectID string) string {
	return filepath.Join(f.BaseDir, projectID)
}

func (f *FileCache) bucketDir(key Key) string {
	return filepath.Join(f.BaseDir, key.ProjectID, namespaceDir(key.Namespace), key.Bucket)
}

func (f *FileCache) tarPath(key Key) string {
	return filepath.Join(f.bucketDir(key), key.InputKey+".tar.zst")
}

func (f *FileCache) metaPath(key Key) string {
	return filepath.Join(f.bucketDir(key), key.InputKey+".meta")
}

func (f *FileCache) latestPath(key Key) string {
	return filepath.Join(f.bucketDir(key), "LATEST")
}

func (f *FileCache) Restore(key Key) (*RestoreResult, error) {
	// Exact content-addressed hit. Open the FD before bumping the meta so an
	// in-flight restore survives a concurrent eviction (open FDs survive
	// unlink on POSIX).
	if file, err := os.Open(f.tarPath(key)); err == nil {
		f.bumpAccess(f.metaPath(key))
		return &RestoreResult{Reader: file, Warm: false, InputKey: key.InputKey}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("open cache tarball: %w", err)
	}

	// Warm start: serve the bucket's most-recently-used entry instead.
	mru, ok, err := f.mruInputKey(key)
	if err != nil || !ok {
		return nil, err
	}
	warmKey := key
	warmKey.InputKey = mru
	file, err := os.Open(f.tarPath(warmKey))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open warm cache tarball: %w", err)
	}
	f.bumpAccess(f.metaPath(warmKey))
	return &RestoreResult{Reader: file, Warm: true, InputKey: mru}, nil
}

// mruInputKey resolves the most-recently-used input key in a bucket other than
// key.InputKey, preferring the LATEST hint and falling back to scanning metas.
func (f *FileCache) mruInputKey(key Key) (string, bool, error) {
	dir := f.bucketDir(key)
	if data, err := os.ReadFile(f.latestPath(key)); err == nil {
		ik := strings.TrimSpace(string(data))
		if ik != "" && ik != key.InputKey {
			if _, statErr := os.Stat(filepath.Join(dir, ik+".tar.zst")); statErr == nil {
				return ik, true, nil
			}
		}
	}

	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var bestIK string
	var bestT time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".meta") {
			continue
		}
		m, err := readMetadata(filepath.Join(dir, e.Name()))
		if err != nil || m.InputKey == key.InputKey {
			continue
		}
		if _, statErr := os.Stat(filepath.Join(dir, m.InputKey+".tar.zst")); statErr != nil {
			continue
		}
		if bestIK == "" || m.LastAccess.After(bestT) {
			bestIK = m.InputKey
			bestT = m.LastAccess
		}
	}
	if bestIK == "" {
		return "", false, nil
	}
	return bestIK, true, nil
}

func (f *FileCache) bumpAccess(metaPath string) {
	m, err := readMetadata(metaPath)
	if err != nil {
		return
	}
	m.LastAccess = f.now()
	_ = writeMetadata(metaPath, m)
}

func (f *FileCache) Save(key Key, data io.Reader) error {
	dir := f.bucketDir(key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create cache dir %s: %w", dir, err)
	}

	// Human-readable marker identifying the project at its root.
	_ = os.WriteFile(filepath.Join(f.projectDir(key.ProjectID), "SOURCE"),
		[]byte(key.ProjectID+"\n"), 0o644)

	dest := f.tarPath(key)
	if _, err := os.Stat(dest); err == nil {
		// Write-once: identical content is already present.
		f.updateLatest(key)
		return nil
	}

	tmp, err := os.CreateTemp(dir, ".cache-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file for cache: %w", err)
	}
	tmpName := tmp.Name()
	n, err := io.Copy(tmp, data)
	if err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write cache tarball: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close cache tarball: %w", err)
	}

	// Guarded rename: a concurrent writer may have won the race. The bytes are
	// identical by construction, so drop our temp and keep theirs.
	if _, err := os.Stat(dest); err == nil {
		os.Remove(tmpName)
	} else if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename cache tarball: %w", err)
	}

	now := f.now()
	_ = writeMetadata(f.metaPath(key), &metadata{
		Namespace:  key.Namespace,
		Bucket:     key.Bucket,
		InputKey:   key.InputKey,
		GuestPath:  key.GuestPath,
		SizeBytes:  n,
		CreatedAt:  now,
		LastAccess: now,
	})
	f.updateLatest(key)
	return nil
}

func (f *FileCache) updateLatest(key Key) {
	_ = atomicWrite(f.latestPath(key), []byte(key.InputKey+"\n"), 0o644)
}

func (f *FileCache) Has(key Key) (bool, error) {
	_, err := os.Stat(f.tarPath(key))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (f *FileCache) List(projectID string) ([]Entry, error) {
	root := f.projectDir(projectID)
	var entries []Entry
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".meta") {
			return nil
		}
		m, err := readMetadata(path)
		if err != nil {
			return nil
		}
		entries = append(entries, Entry{
			Key: Key{
				ProjectID: projectID,
				Namespace: m.Namespace,
				Bucket:    m.Bucket,
				GuestPath: m.GuestPath,
				InputKey:  m.InputKey,
			},
			SizeBytes:  m.SizeBytes,
			CreatedAt:  m.CreatedAt,
			LastAccess: m.LastAccess,
		})
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return entries, nil
}

func (f *FileCache) Clear(projectID string) error {
	return os.RemoveAll(f.projectDir(projectID))
}

// evictItem is one candidate for eviction, captured during the scan pass.
type evictItem struct {
	projectID  string
	tarPath    string
	metaPath   string
	size       int64
	lastAccess time.Time
}

func (f *FileCache) Evict(quota Quota) (EvictReport, error) {
	var report EvictReport

	projectIDs, err := f.projectIDs()
	if errors.Is(err, os.ErrNotExist) {
		return report, nil
	}
	if err != nil {
		return report, err
	}

	// Per-project sweep.
	if quota.PerProjectBytes > 0 {
		for _, pid := range projectIDs {
			freed, removed := f.evictToLimit(pid, f.collectItems(pid), quota.PerProjectBytes)
			report.BytesFreed += freed
			report.RemovedEntries += removed
		}
	}

	// Global sweep over what remains after the per-project pass.
	if quota.GlobalBytes > 0 {
		var all []evictItem
		for _, pid := range projectIDs {
			all = append(all, f.collectItems(pid)...)
		}
		freed, removed := f.evictGlobal(all, quota.GlobalBytes)
		report.BytesFreed += freed
		report.RemovedEntries += removed
	}

	return report, nil
}

func (f *FileCache) projectIDs() ([]string, error) {
	entries, err := os.ReadDir(f.BaseDir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

func (f *FileCache) collectItems(projectID string) []evictItem {
	var items []evictItem
	_ = filepath.WalkDir(f.projectDir(projectID), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".meta") {
			return nil
		}
		m, err := readMetadata(path)
		if err != nil {
			return nil
		}
		items = append(items, evictItem{
			projectID:  projectID,
			tarPath:    strings.TrimSuffix(path, ".meta") + ".tar.zst",
			metaPath:   path,
			size:       m.SizeBytes,
			lastAccess: m.LastAccess,
		})
		return nil
	})
	return items
}

func (f *FileCache) evictToLimit(projectID string, items []evictItem, limit int64) (int64, int) {
	var total int64
	for _, it := range items {
		total += it.size
	}
	if total <= limit {
		return 0, 0
	}
	sortByAccess(items)

	unlock := f.lockProject(projectID)
	defer unlock()

	var freed int64
	var removed int
	for _, it := range items {
		if total-freed <= limit {
			break
		}
		if f.deleteItemLocked(it) {
			freed += it.size
			removed++
		}
	}
	return freed, removed
}

func (f *FileCache) evictGlobal(items []evictItem, limit int64) (int64, int) {
	var total int64
	for _, it := range items {
		total += it.size
	}
	if total <= limit {
		return 0, 0
	}
	sortByAccess(items)

	var freed int64
	var removed int
	for _, it := range items {
		if total-freed <= limit {
			break
		}
		unlock := f.lockProject(it.projectID)
		ok := f.deleteItemLocked(it)
		unlock()
		if ok {
			freed += it.size
			removed++
		}
	}
	return freed, removed
}

// deleteItemLocked removes a tarball and its meta, skipping entries touched
// since the scan. The .tar.zst is removed before the .meta so a crash never
// leaves a meta pointing at a missing tarball.
func (f *FileCache) deleteItemLocked(it evictItem) bool {
	if m, err := readMetadata(it.metaPath); err == nil && m.LastAccess.After(it.lastAccess) {
		return false
	}
	if err := os.Remove(it.tarPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false
	}
	_ = os.Remove(it.metaPath)
	return true
}

func sortByAccess(items []evictItem) {
	sort.Slice(items, func(i, j int) bool {
		return items[i].lastAccess.Before(items[j].lastAccess)
	})
}

// lockProject takes an exclusive advisory flock on <projectID>/.lock for the
// eviction delete pass. Restores never take this lock, so they never block on
// eviction. Best-effort: a failure to lock returns a no-op unlock.
func (f *FileCache) lockProject(projectID string) func() {
	dir := f.projectDir(projectID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return func() {}
	}
	fd, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return func() {}
	}
	if err := syscall.Flock(int(fd.Fd()), syscall.LOCK_EX); err != nil {
		fd.Close()
		return func() {}
	}
	return func() {
		syscall.Flock(int(fd.Fd()), syscall.LOCK_UN)
		fd.Close()
	}
}
