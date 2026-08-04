package apikey

import (
	"context"
	"time"
)

// Wildcard is the project entry that grants a key access to every project.
const Wildcard = "*"

// APIKey is a shared-secret bearer credential scoped to a set of projects.
// Only the hash of the secret part is persisted; the full token is shown once
// at creation time.
type APIKey struct {
	KeyID    string
	Name     string
	Hash     string   // hex-encoded sha256 of the secret part
	Projects []string // [Wildcard] grants access to all projects
	Created  time.Time
	Expires  *time.Time // nil means the key never expires
	Disabled bool
	// The following cap what this key may hold *at once*, summed across the
	// jobs it has running, so one client cannot take the whole host. Each is
	// nil/empty to inherit the [scheduler.per_key] default, and an explicit
	// zero to mean unlimited even when a default is set. The cap is per key,
	// not per project: a key driving ten projects is still held to one total.
	MaxJobs   *int
	MaxCPUs   *uint
	MaxMemory string
	MaxDisk   string
}

// AllowsProject reports whether the key is scoped to the named project, either
// explicitly or via the wildcard entry.
func (k *APIKey) AllowsProject(name string) bool {
	for _, p := range k.Projects {
		if p == Wildcard || p == name {
			return true
		}
	}
	return false
}

// Expired reports whether the key has an expiry that is now in the past.
func (k *APIKey) Expired(now time.Time) bool {
	return k.Expires != nil && now.After(*k.Expires)
}

// Store provides CRUD operations for API keys, keyed by their ID. Get and
// Delete return tomlstore.ErrNotFound when no key matches.
type Store interface {
	Get(ctx context.Context, keyID string) (*APIKey, error)
	List(ctx context.Context) ([]*APIKey, error)
	Put(ctx context.Context, k *APIKey) error
	Delete(ctx context.Context, keyID string) error
}
