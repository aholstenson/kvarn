package apikey

import (
	"context"
	"errors"
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

// ErrNotFound is returned by Store.Get when no key matches the given ID. It is
// a distinct sentinel so callers can tell "unknown key" (reject) apart from a
// store or parse failure (fail closed).
var ErrNotFound = errors.New("api key not found")

// Store provides CRUD operations for API keys, keyed by their ID.
type Store interface {
	Get(ctx context.Context, keyID string) (*APIKey, error) // ErrNotFound if missing
	List(ctx context.Context) ([]*APIKey, error)
	Put(ctx context.Context, k *APIKey) error
	Delete(ctx context.Context, keyID string) error
}
