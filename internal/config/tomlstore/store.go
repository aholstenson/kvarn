// Package tomlstore implements a generic, hot-reloaded TOML file-backed store.
//
// Each operation re-reads the backing file so out-of-band edits (made by a
// separate CLI invocation, for example) are visible without restart. Writes
// are atomic (temp file + rename) and serialized across processes via an
// advisory flock on "<path>.lock", so a concurrent read never sees a partial
// file and two writers cannot lose each other's edits.
//
// The Store is parameterized by:
//
//   - K: the lookup key (string for most stores, a composite struct for secrets)
//   - FD: the on-disk file shape (anything go-toml/v2 can unmarshal)
//   - E:  the per-entry value inside FD
//   - D:  the domain type the wrapper package exposes
//
// Domain ↔ entry conversion is delegated to the wrapper so domain packages do
// not need to import this one for purely on-disk concerns.
package tomlstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/pelletier/go-toml/v2"

	"github.com/aholstenson/kvarn/internal/config/atomicfile"
)

// ErrNotFound is returned by Get and Delete when no entry matches the given
// key. Callers use errors.Is(err, tomlstore.ErrNotFound) to tell "absent" apart
// from a parse or I/O failure (fail closed).
var ErrNotFound = errors.New("not found")

// Mode bundles the file and parent-directory permissions for a store. Two
// presets cover every existing user-config file; new stores should reuse them
// rather than introduce a third variant.
type Mode struct {
	File os.FileMode
	Dir  os.FileMode
}

var (
	// Config is the permission preset for non-sensitive user config.
	Config = Mode{File: 0o644, Dir: 0o755}
	// Secret is the permission preset for files holding secret material
	// (hashed credentials, bearer tokens, secret values). Mode 0600 prevents
	// other users on the host from reading the file.
	Secret = Mode{File: 0o600, Dir: 0o755}
)

// Schema describes how the generic store reads, mutates, and writes the
// on-disk FD value. Operating on FD directly (rather than a flat map) lets a
// store with a separate [defaults] block (forge, model) or a nested map
// (secret) share this implementation without forcing a uniform on-disk shape.
type Schema[K comparable, FD any, E any] struct {
	// NewFileData returns a fresh, zero-value FD. Used when the backing file
	// does not yet exist; should pre-allocate any nested maps the rest of the
	// schema callbacks assume are non-nil.
	NewFileData func() FD
	// Get fetches the entry for k, returning ok=false if absent.
	Get func(fd FD, k K) (E, bool)
	// Put inserts or replaces the entry for k.
	Put func(fd *FD, k K, e E)
	// Delete removes the entry for k, returning whether it existed.
	Delete func(fd *FD, k K) bool
	// Keys returns every key currently in fd. Order is irrelevant; the store
	// sorts via Less before returning a List result.
	Keys func(fd FD) []K
	// Less defines the canonical sort order for List.
	Less func(a, b K) bool
}

// Store is the generic hot-reloaded TOML store.
type Store[K comparable, FD any, E any, D any] struct {
	path   string
	mode   Mode
	schema Schema[K, FD, E]

	entryToDomain func(K, E) (D, error)
	domainToEntry func(D) (K, E)

	mu sync.RWMutex
}

// New constructs a Store. The entryToDomain / domainToEntry callbacks adapt
// the on-disk entry shape to the domain type the wrapper package exposes;
// keeping them outside the schema lets multiple stores share the same FD
// layout while producing different domain types.
func New[K comparable, FD any, E any, D any](
	path string,
	mode Mode,
	schema Schema[K, FD, E],
	entryToDomain func(K, E) (D, error),
	domainToEntry func(D) (K, E),
) *Store[K, FD, E, D] {
	return &Store[K, FD, E, D]{
		path:          path,
		mode:          mode,
		schema:        schema,
		entryToDomain: entryToDomain,
		domainToEntry: domainToEntry,
	}
}

// Path returns the backing file path.
func (s *Store[K, FD, E, D]) Path() string {
	return s.path
}

// Load reads the backing file and returns the parsed FD. A missing file
// yields schema.NewFileData(); any other read or parse error is bubbled
// wrapped with the file path.
//
// Exposed so wrappers that store auxiliary on-disk blocks (forge defaults,
// model defaults) can pull those fields out without duplicating the load.
func (s *Store[K, FD, E, D]) Load(_ context.Context) (FD, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.load()
}

func (s *Store[K, FD, E, D]) load() (FD, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s.schema.NewFileData(), nil
		}
		var zero FD
		return zero, err
	}

	fd := s.schema.NewFileData()
	if err := toml.Unmarshal(data, &fd); err != nil {
		var zero FD
		return zero, fmt.Errorf("parse %s: %w", s.path, err)
	}
	return fd, nil
}

func (s *Store[K, FD, E, D]) save(fd FD) error {
	if err := os.MkdirAll(filepath.Dir(s.path), s.mode.Dir); err != nil {
		return err
	}
	data, err := toml.Marshal(fd)
	if err != nil {
		return err
	}
	return atomicfile.Write(s.path, data, s.mode.File)
}

// Get returns the domain value for k, or ErrNotFound if absent.
func (s *Store[K, FD, E, D]) Get(_ context.Context, k K) (D, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fd, err := s.load()
	if err != nil {
		var zero D
		return zero, err
	}

	entry, ok := s.schema.Get(fd, k)
	if !ok {
		var zero D
		return zero, ErrNotFound
	}
	return s.entryToDomain(k, entry)
}

// List returns every domain value in canonical sort order. The result is a
// preallocated, non-nil slice even when the file is missing or empty so
// callers never need to nil-check.
func (s *Store[K, FD, E, D]) List(_ context.Context) ([]D, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fd, err := s.load()
	if err != nil {
		return nil, err
	}

	keys := s.schema.Keys(fd)
	sort.Slice(keys, func(i, j int) bool { return s.schema.Less(keys[i], keys[j]) })

	result := make([]D, 0, len(keys))
	for _, k := range keys {
		entry, ok := s.schema.Get(fd, k)
		if !ok {
			continue
		}
		d, err := s.entryToDomain(k, entry)
		if err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, nil
}

// Put writes d to the backing file. The load → mutate → save sequence runs
// under both the in-process Mutex and a cross-process flock on "<path>.lock".
func (s *Store[K, FD, E, D]) Put(_ context.Context, d D) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return atomicfile.WithLock(s.path, func() error {
		fd, err := s.load()
		if err != nil {
			return err
		}
		k, e := s.domainToEntry(d)
		s.schema.Put(&fd, k, e)
		return s.save(fd)
	})
}

// Delete removes the entry for k. ErrNotFound is returned when no such entry
// exists, mirroring Get.
func (s *Store[K, FD, E, D]) Delete(_ context.Context, k K) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return atomicfile.WithLock(s.path, func() error {
		fd, err := s.load()
		if err != nil {
			return err
		}
		if !s.schema.Delete(&fd, k) {
			return ErrNotFound
		}
		return s.save(fd)
	})
}
