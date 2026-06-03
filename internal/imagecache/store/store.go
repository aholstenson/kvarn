// Package store implements a content-addressed on-disk cache for OCI manifests
// and blobs, used by the orchestrator's pull-through image cache.
//
// Blobs are immutable (their digest is the file name), so all writers race to
// produce identical bytes and a temp-file-plus-rename keeps the destination
// either absent or fully populated. Manifests are split into mutable tag refs
// (with a TTL sidecar) and immutable digest refs.
package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Store is the on-disk image cache.
type Store struct {
	BaseDir string

	// Clock, when set, supplies the current time. Defaults to time.Now; tests
	// override it for deterministic ordering.
	Clock func() time.Time

	// hits / misses are in-process counters surfaced by `kvarn image-cache
	// stats`. They reset on orchestrator restart, matching the existing tool
	// cache's "no persistent stats" posture.
	blobHits   atomic.Int64
	blobMisses atomic.Int64
	manHits    atomic.Int64
	manMisses  atomic.Int64
}

// DefaultDir returns the on-disk root for the image cache. Sits next to the
// VM disk image cache (~/.cache/kvarn/images/) without colliding: that path
// is per-version-and-arch under `images/`, this one lives under
// `image-cache/`.
func DefaultDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("determine user cache dir: %w", err)
	}
	return filepath.Join(dir, "kvarn", "image-cache"), nil
}

// New constructs a Store rooted at dir.
func New(dir string) *Store {
	return &Store{BaseDir: dir}
}

func (s *Store) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

// ParseDigest validates a digest string of the form "sha256:<hex>" and
// returns its algorithm and hex components. Only sha256 is supported in v1
// because every modern registry uses it; rejecting unknown algorithms early
// keeps the on-disk layout simple.
func ParseDigest(d string) (alg, hex string, err error) {
	parts := strings.SplitN(d, ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid digest %q", d)
	}
	if parts[0] != "sha256" {
		return "", "", fmt.Errorf("unsupported digest algorithm %q", parts[0])
	}
	if len(parts[1]) != 64 {
		return "", "", fmt.Errorf("invalid sha256 hex length")
	}
	for _, c := range parts[1] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return "", "", fmt.Errorf("invalid sha256 hex")
		}
	}
	return parts[0], parts[1], nil
}

// BlobPath returns the on-disk path for a blob digest. The directory may not
// exist yet; the caller is expected to create it on write.
func (s *Store) BlobPath(digest string) (string, error) {
	alg, hex, err := ParseDigest(digest)
	if err != nil {
		return "", err
	}
	// Two-level sharding by the first two hex chars matches the OCI image
	// layout spec and keeps any single directory small.
	return filepath.Join(s.BaseDir, "blobs", alg, hex[:2], hex), nil
}

func (s *Store) blobMetaPath(digest string) (string, error) {
	p, err := s.BlobPath(digest)
	if err != nil {
		return "", err
	}
	return p + ".meta", nil
}

// OpenBlob returns a reader for the cached blob along with its recorded size.
// A miss returns (nil, 0, false, nil).
func (s *Store) OpenBlob(digest string) (io.ReadCloser, int64, bool, error) {
	p, err := s.BlobPath(digest)
	if err != nil {
		return nil, 0, false, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.blobMisses.Add(1)
			return nil, 0, false, nil
		}
		return nil, 0, false, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, false, err
	}
	mp, _ := s.blobMetaPath(digest)
	s.bumpBlobAccess(mp)
	s.blobHits.Add(1)
	return f, info.Size(), true, nil
}

// HasBlob reports whether a blob is present.
func (s *Store) HasBlob(digest string) (bool, int64, error) {
	p, err := s.BlobPath(digest)
	if err != nil {
		return false, 0, err
	}
	info, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, 0, nil
		}
		return false, 0, err
	}
	return true, info.Size(), nil
}

// WriteBlob streams r into the cache under digest. Content is written to a
// temp file in the destination directory, then renamed into place — a
// concurrent writer racing to produce identical bytes is safe; the loser
// drops its temp file.
//
// The returned size is the number of bytes written. Callers should compare
// it against the expected upstream Content-Length and treat a mismatch as a
// hard error, since the blob payload is what holds the integrity guarantee.
func (s *Store) WriteBlob(digest string, r io.Reader) (int64, error) {
	p, err := s.BlobPath(digest)
	if err != nil {
		return 0, err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("mkdir blob dir: %w", err)
	}

	if info, err := os.Stat(p); err == nil {
		// Already cached; drain the reader so the upstream connection can
		// be reused, but return the existing size.
		_, _ = io.Copy(io.Discard, r)
		return info.Size(), nil
	}

	tmp, err := os.CreateTemp(dir, ".blob-*.tmp")
	if err != nil {
		return 0, fmt.Errorf("create temp blob: %w", err)
	}
	tmpName := tmp.Name()
	n, err := io.Copy(tmp, r)
	if err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return 0, fmt.Errorf("write blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return 0, fmt.Errorf("close blob: %w", err)
	}
	if _, err := os.Stat(p); err == nil {
		os.Remove(tmpName)
	} else if err := os.Rename(tmpName, p); err != nil {
		os.Remove(tmpName)
		return 0, fmt.Errorf("rename blob: %w", err)
	}

	now := s.now()
	mp, _ := s.blobMetaPath(digest)
	_ = writeJSON(mp, &blobMeta{
		Digest:     digest,
		SizeBytes:  n,
		CreatedAt:  now,
		LastAccess: now,
	})
	return n, nil
}

func (s *Store) bumpBlobAccess(metaPath string) {
	var m blobMeta
	if err := readJSON(metaPath, &m); err != nil {
		return
	}
	m.LastAccess = s.now()
	_ = writeJSON(metaPath, &m)
}

// --- Manifests ---

func (s *Store) manifestPaths(registry, name, ref string) (body, meta string, err error) {
	if registry == "" || name == "" || ref == "" {
		return "", "", fmt.Errorf("registry, name, ref are required")
	}
	if strings.ContainsAny(registry+name+ref, "\x00") || strings.Contains(ref, "/") {
		return "", "", fmt.Errorf("invalid manifest path component")
	}
	dir := filepath.Join(s.BaseDir, "manifests", safeComponent(registry), filepath.FromSlash(name))
	body = filepath.Join(dir, safeComponent(ref)+".json")
	meta = filepath.Join(dir, safeComponent(ref)+".meta")
	return body, meta, nil
}

// safeComponent neutralizes characters that would otherwise traverse the
// filesystem or collide with system files. Only ASCII alphanumerics, dot,
// dash, and underscore survive; everything else is replaced with '_'.
func safeComponent(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// ManifestEntry describes a cached manifest.
type ManifestEntry struct {
	Body        []byte
	Meta        manifestMeta
	BodyPath    string
	MetaPath    string
	ContentType string
}

// ReadManifest returns the cached manifest for (registry, name, ref), or
// (nil, false) on a miss. The returned entry is always returned even when
// stale — the caller decides whether to revalidate or serve as-is.
func (s *Store) ReadManifest(registry, name, ref string) (*ManifestEntry, bool, error) {
	bodyPath, metaPath, err := s.manifestPaths(registry, name, ref)
	if err != nil {
		return nil, false, err
	}
	var meta manifestMeta
	if err := readJSON(metaPath, &meta); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.manMisses.Add(1)
			return nil, false, nil
		}
		return nil, false, err
	}
	body, err := os.ReadFile(bodyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.manMisses.Add(1)
			return nil, false, nil
		}
		return nil, false, err
	}
	meta.LastAccess = s.now()
	_ = writeJSON(metaPath, &meta)
	s.manHits.Add(1)
	return &ManifestEntry{
		Body:        body,
		Meta:        meta,
		BodyPath:    bodyPath,
		MetaPath:    metaPath,
		ContentType: meta.ContentType,
	}, true, nil
}

// WriteManifest stores body under (registry, name, ref) with the given
// metadata. For tag refs, ttl controls how long the entry is served without
// revalidation; pass 0 for digest refs (immutable, never expire).
func (s *Store) WriteManifest(registry, name, ref string, body []byte, resolvedDigest, contentType, etag string, isTag bool, ttl time.Duration) error {
	bodyPath, metaPath, err := s.manifestPaths(registry, name, ref)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(bodyPath), 0o755); err != nil {
		return fmt.Errorf("mkdir manifest dir: %w", err)
	}
	if err := atomicWrite(bodyPath, body, 0o644); err != nil {
		return fmt.Errorf("write manifest body: %w", err)
	}
	now := s.now()
	meta := manifestMeta{
		Registry:       registry,
		Name:           name,
		Ref:            ref,
		ResolvedDigest: resolvedDigest,
		ContentType:    contentType,
		SizeBytes:      int64(len(body)),
		IsTag:          isTag,
		FetchedAt:      now,
		LastAccess:     now,
		ETag:           etag,
	}
	if isTag && ttl > 0 {
		meta.ExpiresAt = now.Add(ttl)
	}
	return writeJSON(metaPath, &meta)
}

// IsManifestFresh reports whether the entry is still within its TTL window.
// Digest manifests (IsTag=false) are always fresh.
func (s *Store) IsManifestFresh(m manifestMeta) bool {
	if !m.IsTag {
		return true
	}
	if m.ExpiresAt.IsZero() {
		return false
	}
	return s.now().Before(m.ExpiresAt)
}

// --- Stats ---

// Stats holds cache counters and totals.
type Stats struct {
	BlobBytes     int64
	BlobCount     int
	ManifestCount int
	BlobHits      int64
	BlobMisses    int64
	ManifestHits  int64
	ManifestMiss  int64
}

// Stats walks the on-disk store to compute totals. The hit/miss counters are
// in-memory and reflect activity since the orchestrator started.
func (s *Store) Stats() (Stats, error) {
	st := Stats{
		BlobHits:     s.blobHits.Load(),
		BlobMisses:   s.blobMisses.Load(),
		ManifestHits: s.manHits.Load(),
		ManifestMiss: s.manMisses.Load(),
	}
	blobsRoot := filepath.Join(s.BaseDir, "blobs")
	_ = filepath.WalkDir(blobsRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || strings.HasSuffix(d.Name(), ".meta") || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		st.BlobBytes += info.Size()
		st.BlobCount++
		return nil
	})
	manifestsRoot := filepath.Join(s.BaseDir, "manifests")
	_ = filepath.WalkDir(manifestsRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".meta") {
			return nil
		}
		st.ManifestCount++
		return nil
	})
	return st, nil
}

// ManifestInfo is one row in the manifest listing.
type ManifestInfo struct {
	Registry   string
	Name       string
	Ref        string
	Digest     string
	SizeBytes  int64
	IsTag      bool
	FetchedAt  time.Time
	LastAccess time.Time
}

// ListManifests returns every cached manifest. Used by `kvarn image-cache
// list`.
func (s *Store) ListManifests() ([]ManifestInfo, error) {
	root := filepath.Join(s.BaseDir, "manifests")
	var out []ManifestInfo
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
		var m manifestMeta
		if err := readJSON(path, &m); err != nil {
			return nil
		}
		out = append(out, ManifestInfo{
			Registry:   m.Registry,
			Name:       m.Name,
			Ref:        m.Ref,
			Digest:     m.ResolvedDigest,
			SizeBytes:  m.SizeBytes,
			IsTag:      m.IsTag,
			FetchedAt:  m.FetchedAt,
			LastAccess: m.LastAccess,
		})
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return out, nil
}

// Clear removes every cached blob and manifest. Used by `kvarn image-cache
// clear --all`. Repo-scoped clears use ClearRepo.
func (s *Store) Clear() error {
	for _, sub := range []string{"blobs", "manifests"} {
		if err := os.RemoveAll(filepath.Join(s.BaseDir, sub)); err != nil {
			return err
		}
	}
	return nil
}

// ClearRepo removes every cached manifest whose name matches repo. Blobs are
// shared and intentionally untouched: another repo's manifest may still
// reference them.
func (s *Store) ClearRepo(repo string) error {
	manifests, err := s.ListManifests()
	if err != nil {
		return err
	}
	for _, m := range manifests {
		if m.Name != repo {
			continue
		}
		body, meta, err := s.manifestPaths(m.Registry, m.Name, m.Ref)
		if err != nil {
			continue
		}
		os.Remove(body)
		os.Remove(meta)
	}
	return nil
}

// --- Eviction ---

// EvictReport summarizes a sweep.
type EvictReport struct {
	BytesFreed     int64
	RemovedEntries int
}

type blobEvictItem struct {
	digest     string
	blobPath   string
	metaPath   string
	size       int64
	lastAccess time.Time
}

// EvictGlobal trims the blob store to limit bytes, removing the
// least-recently-used blobs first. Manifests are not part of the sweep —
// they're cheap and rarely the pressure point.
//
// The sweep takes a global flock on .lock so a concurrent CLI sweep can't
// double-delete the same entries.
func (s *Store) EvictGlobal(limit int64) (EvictReport, error) {
	var rep EvictReport
	if limit <= 0 {
		return rep, nil
	}
	unlock, err := s.lock()
	if err != nil {
		return rep, err
	}
	defer unlock()

	items := s.collectBlobs()
	var total int64
	for _, it := range items {
		total += it.size
	}
	if total <= limit {
		return rep, nil
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].lastAccess.Before(items[j].lastAccess)
	})
	for _, it := range items {
		if total-rep.BytesFreed <= limit {
			break
		}
		// Re-check meta in case a concurrent serve has just bumped it.
		var m blobMeta
		if err := readJSON(it.metaPath, &m); err == nil && m.LastAccess.After(it.lastAccess) {
			continue
		}
		if err := os.Remove(it.blobPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			continue
		}
		_ = os.Remove(it.metaPath)
		rep.BytesFreed += it.size
		rep.RemovedEntries++
	}
	return rep, nil
}

func (s *Store) collectBlobs() []blobEvictItem {
	root := filepath.Join(s.BaseDir, "blobs")
	var items []blobEvictItem
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".meta") {
			return nil
		}
		var m blobMeta
		if err := readJSON(path, &m); err != nil {
			return nil
		}
		blobPath := strings.TrimSuffix(path, ".meta")
		if _, err := os.Stat(blobPath); err != nil {
			// Orphan meta — drop it, treat as already evicted.
			_ = os.Remove(path)
			return nil
		}
		items = append(items, blobEvictItem{
			digest:     m.Digest,
			blobPath:   blobPath,
			metaPath:   path,
			size:       m.SizeBytes,
			lastAccess: m.LastAccess,
		})
		return nil
	})
	return items
}

// lock takes an exclusive advisory flock on BaseDir/.lock. Used by the
// eviction sweep so a CLI + orchestrator concurrent eviction can't
// double-delete an entry. Best-effort: a failure to lock returns a no-op
// unlock.
func (s *Store) lock() (func(), error) {
	if err := os.MkdirAll(s.BaseDir, 0o755); err != nil {
		return func() {}, err
	}
	fd, err := os.OpenFile(filepath.Join(s.BaseDir, ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return func() {}, err
	}
	if err := syscall.Flock(int(fd.Fd()), syscall.LOCK_EX); err != nil {
		fd.Close()
		return func() {}, err
	}
	return func() {
		syscall.Flock(int(fd.Fd()), syscall.LOCK_UN)
		fd.Close()
	}, nil
}
