// Package store implements the on-disk half of the host-side Nix binary
// cache. It holds two kinds of object a Nix substituter serves: narinfo
// records, which describe one store path, and NAR archives, which carry its
// contents.
//
// Both are immutable once published. A NAR is committed only once its bytes
// hash to the value its narinfo declares for them, so any file that is present
// is known to be intact. A narinfo describes a store path whose name is itself
// a hash of what built it, so an upstream never publishes a different record
// under the same name. That is what makes the cache safe to share across every
// project on the host with no invalidation: a hit is correct by construction,
// and the only reason to remove anything is disk space.
package store

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aholstenson/kvarn/internal/config/atomicfile"
	"github.com/aholstenson/kvarn/internal/nixcache/nixbase32"
)

const (
	// storePathHashLen is the length of the hash part of a store path
	// (/nix/store/<hash>-<name>), which is what names a narinfo.
	storePathHashLen = 32

	// narFileHashLen is the length of a sha256 hash in Nix base32, which is
	// what names a NAR file in a binary cache.
	narFileHashLen = 52
)

// ErrHashMismatch is returned by WriteNar when the bytes written do not hash
// to the value expected of them. Nothing is committed in that case.
var ErrHashMismatch = errors.New("nar content does not match its file hash")

// Store is the on-disk Nix binary cache.
type Store struct {
	BaseDir string

	// Clock, when set, supplies the current time. Defaults to time.Now; tests
	// override it for deterministic eviction order.
	Clock func() time.Time

	// In-process counters surfaced by `kvarn nix-cache stats`. They reset
	// when the orchestrator restarts.
	narHits     atomic.Int64
	narMisses   atomic.Int64
	narInfoHits atomic.Int64
	narInfoMiss atomic.Int64
}

// DefaultDir returns the on-disk root for the Nix cache. It sits next to the
// OCI image cache under the user cache directory.
func DefaultDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("determine user cache dir: %w", err)
	}
	return filepath.Join(dir, "kvarn", "nix-cache"), nil
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

// --- narinfo ---

// ValidStorePathHash reports whether hash has the shape of the hash part of
// a store path: 32 Nix base32 digits.
func ValidStorePathHash(hash string) bool {
	return nixbase32.IsValid(hash, storePathHashLen)
}

func (s *Store) narInfoPath(upstream, hash string) (string, error) {
	if !validUpstream(upstream) {
		return "", fmt.Errorf("invalid upstream name %q", upstream)
	}
	if !ValidStorePathHash(hash) {
		return "", fmt.Errorf("invalid store path hash %q", hash)
	}
	return filepath.Join(s.BaseDir, "narinfo", upstream, hash+".narinfo"), nil
}

// validUpstream accepts a hostname-shaped upstream key: the on-disk layout
// keys narinfo records by the cache they came from, and this is what stops a
// request path from naming a directory outside the store.
func validUpstream(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// ReadNarInfo returns the cached narinfo for a store path hash as served by
// upstream. A miss returns (nil, false, nil).
func (s *Store) ReadNarInfo(upstream, hash string) ([]byte, bool, error) {
	p, err := s.narInfoPath(upstream, hash)
	if err != nil {
		return nil, false, err
	}
	body, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.narInfoMiss.Add(1)
			return nil, false, nil
		}
		return nil, false, err
	}
	s.narInfoHits.Add(1)
	return body, true, nil
}

// WriteNarInfo stores a narinfo record for a store path hash under upstream.
func (s *Store) WriteNarInfo(upstream, hash string, body []byte) error {
	p, err := s.narInfoPath(upstream, hash)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("mkdir narinfo dir: %w", err)
	}
	return atomicfile.Write(p, body, 0o644)
}

// --- NAR files ---

// ValidNarFileHash reports whether hash has the shape of the hash naming a
// NAR file: 52 Nix base32 digits, a sha256.
func ValidNarFileHash(hash string) bool {
	return nixbase32.IsValid(hash, narFileHashLen)
}

// ParseNarName splits a NAR file name of the form <hash>.nar[.<ext>] into its
// hash and reports whether the name has that shape. What that hash covers is
// the publishing cache's choice — cache.nixos.org names a NAR after the
// uncompressed archive while serving it compressed — so the name identifies a
// file but is not on its own a checksum of the bytes served under it.
func ParseNarName(name string) (hash string, ok bool) {
	if len(name) < narFileHashLen+len(".nar") {
		return "", false
	}
	hash = name[:narFileHashLen]
	if !nixbase32.IsValid(hash, narFileHashLen) {
		return "", false
	}
	rest := name[narFileHashLen:]
	if !strings.HasPrefix(rest, ".nar") {
		return "", false
	}
	ext := strings.TrimPrefix(rest, ".nar")
	if ext == "" {
		return hash, true
	}
	if ext[0] != '.' || len(ext) < 2 || len(ext) > 8 {
		return "", false
	}
	for i := 1; i < len(ext); i++ {
		c := ext[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return "", false
		}
	}
	return hash, true
}

// NarPath returns the on-disk path for a NAR file name.
func (s *Store) NarPath(name string) (string, error) {
	hash, ok := ParseNarName(name)
	if !ok {
		return "", fmt.Errorf("invalid nar file name %q", name)
	}
	// Shard by the first two digits so no single directory grows large.
	return filepath.Join(s.BaseDir, "nar", hash[:2], name), nil
}

func (s *Store) narMetaPath(name string) (string, error) {
	p, err := s.NarPath(name)
	if err != nil {
		return "", err
	}
	return p + ".meta", nil
}

// OpenNar returns a seekable reader for the cached NAR along with its size.
// A miss returns (nil, 0, false, nil).
func (s *Store) OpenNar(name string) (io.ReadSeekCloser, int64, bool, error) {
	p, err := s.NarPath(name)
	if err != nil {
		return nil, 0, false, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.narMisses.Add(1)
			return nil, 0, false, nil
		}
		return nil, 0, false, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, false, err
	}
	mp, _ := s.narMetaPath(name)
	s.bumpNarAccess(mp)
	s.narHits.Add(1)
	return f, info.Size(), true, nil
}

// WriteNar streams r into the cache under name. The bytes are hashed as they
// are written and the file is only renamed into place when that hash matches
// expectFileHash, the Nix base32 sha256 the narinfo pointing at this NAR
// declares for the bytes on the wire; otherwise the temp file is dropped and
// ErrHashMismatch is returned. An empty expectFileHash falls back to the hash
// in the file name, which is what a cache that names a NAR after its
// transferred bytes publishes. The returned size is the number of bytes read
// from r either way.
//
// Concurrent writers of the same name are safe: each writes its own temp file
// and the losing rename is discarded.
func (s *Store) WriteNar(name, expectFileHash string, r io.Reader) (int64, error) {
	hash, ok := ParseNarName(name)
	if !ok {
		return 0, fmt.Errorf("invalid nar file name %q", name)
	}
	want := expectFileHash
	if want == "" {
		want = hash
	}
	p, err := s.NarPath(name)
	if err != nil {
		return 0, err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("mkdir nar dir: %w", err)
	}

	if info, err := os.Stat(p); err == nil {
		// Already cached: drain the reader so the upstream connection can be
		// reused, and report the existing size.
		_, _ = io.Copy(io.Discard, r)
		return info.Size(), nil
	}

	tmp, err := os.CreateTemp(dir, ".nar-*.tmp")
	if err != nil {
		return 0, fmt.Errorf("create temp nar: %w", err)
	}
	tmpName := tmp.Name()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return n, fmt.Errorf("write nar: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return n, fmt.Errorf("close nar: %w", err)
	}
	if nixbase32.Encode(h.Sum(nil)) != want {
		os.Remove(tmpName)
		return n, ErrHashMismatch
	}
	if _, err := os.Stat(p); err == nil {
		os.Remove(tmpName)
	} else if err := os.Rename(tmpName, p); err != nil {
		os.Remove(tmpName)
		return n, fmt.Errorf("rename nar: %w", err)
	}

	now := s.now()
	mp, _ := s.narMetaPath(name)
	_ = writeJSON(mp, &narMeta{
		Name:       name,
		SizeBytes:  n,
		CreatedAt:  now,
		LastAccess: now,
	})
	return n, nil
}

// narMeta is the JSON sidecar next to each NAR file. Last access is tracked
// here rather than through the filesystem atime, which is unreliable under
// noatime and relatime mounts, so the LRU sweep removes true cold entries.
type narMeta struct {
	Name       string    `json:"name"`
	SizeBytes  int64     `json:"sizeBytes"`
	CreatedAt  time.Time `json:"createdAt"`
	LastAccess time.Time `json:"lastAccess"`
}

func (s *Store) bumpNarAccess(metaPath string) {
	var m narMeta
	if err := readJSON(metaPath, &m); err != nil {
		return
	}
	m.LastAccess = s.now()
	_ = writeJSON(metaPath, &m)
}

// --- Stats ---

// Stats holds cache totals and counters.
type Stats struct {
	NarBytes      int64
	NarCount      int
	NarInfoCount  int
	NarHits       int64
	NarMisses     int64
	NarInfoHits   int64
	NarInfoMisses int64
}

// Stats walks the on-disk store to compute totals. The hit and miss counters
// are in-memory and reflect activity since the orchestrator started.
func (s *Store) Stats() (Stats, error) {
	st := Stats{
		NarHits:       s.narHits.Load(),
		NarMisses:     s.narMisses.Load(),
		NarInfoHits:   s.narInfoHits.Load(),
		NarInfoMisses: s.narInfoMiss.Load(),
	}
	_ = filepath.WalkDir(filepath.Join(s.BaseDir, "nar"), func(path string, d os.DirEntry, err error) error {
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
		st.NarBytes += info.Size()
		st.NarCount++
		return nil
	})
	_ = filepath.WalkDir(filepath.Join(s.BaseDir, "narinfo"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".narinfo") {
			return nil
		}
		st.NarInfoCount++
		return nil
	})
	return st, nil
}

// Clear removes every cached narinfo and NAR.
func (s *Store) Clear() error {
	for _, sub := range []string{"nar", "narinfo"} {
		if err := os.RemoveAll(filepath.Join(s.BaseDir, sub)); err != nil {
			return err
		}
	}
	return nil
}

// --- Eviction ---

// EvictReport summarizes a sweep.
type EvictReport struct {
	BytesFreed     int64
	RemovedEntries int
}

type narEvictItem struct {
	narPath    string
	metaPath   string
	size       int64
	lastAccess time.Time
}

// EvictGlobal trims the NAR files to limit bytes, least recently used first.
// Narinfo records are not swept: they are small, and a record whose NAR has
// been evicted still serves its purpose, because the NAR is fetched again on
// demand.
//
// The sweep holds an advisory lock on the store so a CLI sweep and the
// orchestrator's own do not remove the same entries at once.
func (s *Store) EvictGlobal(limit int64) (EvictReport, error) {
	var rep EvictReport
	if limit <= 0 {
		return rep, nil
	}
	if err := os.MkdirAll(s.BaseDir, 0o755); err != nil {
		return rep, err
	}
	err := atomicfile.WithLock(filepath.Join(s.BaseDir, "evict"), func() error {
		items := s.collectNars()
		var total int64
		for _, it := range items {
			total += it.size
		}
		if total <= limit {
			return nil
		}
		sort.Slice(items, func(i, j int) bool {
			return items[i].lastAccess.Before(items[j].lastAccess)
		})
		for _, it := range items {
			if total-rep.BytesFreed <= limit {
				break
			}
			// Skip an entry a concurrent serve has just touched.
			var m narMeta
			if err := readJSON(it.metaPath, &m); err == nil && m.LastAccess.After(it.lastAccess) {
				continue
			}
			if err := os.Remove(it.narPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				continue
			}
			_ = os.Remove(it.metaPath)
			rep.BytesFreed += it.size
			rep.RemovedEntries++
		}
		return nil
	})
	return rep, err
}

func (s *Store) collectNars() []narEvictItem {
	var items []narEvictItem
	_ = filepath.WalkDir(filepath.Join(s.BaseDir, "nar"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".meta") {
			return nil
		}
		var m narMeta
		if err := readJSON(path, &m); err != nil {
			return nil
		}
		narPath := strings.TrimSuffix(path, ".meta")
		if _, err := os.Stat(narPath); err != nil {
			// A sidecar without its file: drop it and treat as evicted.
			_ = os.Remove(path)
			return nil
		}
		items = append(items, narEvictItem{
			narPath:    narPath,
			metaPath:   path,
			size:       m.SizeBytes,
			lastAccess: m.LastAccess,
		})
		return nil
	})
	return items
}

func readJSON(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("parse meta %s: %w", path, err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return atomicfile.Write(path, data, 0o644)
}
