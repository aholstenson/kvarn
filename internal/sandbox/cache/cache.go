package cache

import (
	"io"
	"time"
)

// RestoreResult is returned by Restore on a cache hit.
//
// Warm distinguishes an exact content-addressed hit (Warm=false) from a
// most-recently-used fallback within the same bucket (Warm=true), which is
// served when the exact InputKey is absent but a sibling entry exists.
type RestoreResult struct {
	Reader   io.ReadCloser
	Warm     bool
	InputKey string
}

// Entry describes a single stored cache layer, for inspection and eviction.
type Entry struct {
	Key        Key
	SizeBytes  int64
	CreatedAt  time.Time
	LastAccess time.Time
}

// Quota bounds cache disk usage. A zero field means "no limit" for that
// dimension.
type Quota struct {
	PerProjectBytes int64
	GlobalBytes     int64
}

// EvictReport summarizes a single eviction sweep.
type EvictReport struct {
	RemovedEntries int
	BytesFreed     int64
}

// Provider stores tool caches as content-addressed, write-once tarballs keyed
// by (ProjectID, Namespace, Bucket, InputKey).
type Provider interface {
	// Restore returns cached data for a key, or nil on a miss. A non-nil
	// result with Warm=true means an exact-key miss was served from the
	// bucket's most-recently-used entry.
	Restore(key Key) (*RestoreResult, error)
	// Save stores cached data for a key. Write-once: a no-op if the key is
	// already present (Has(key) == true).
	Save(key Key, data io.Reader) error
	// Has reports whether an exact entry exists for the key.
	Has(key Key) (bool, error)
	// List returns all stored entries for a project.
	List(projectID string) ([]Entry, error)
	// Clear removes all cached data for a project.
	Clear(projectID string) error
	// Evict runs an LRU sweep to bring usage within quota.
	Evict(quota Quota) (EvictReport, error)
}
