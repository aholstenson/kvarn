package tomlstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aholstenson/kvarn/internal/config/apikey"
	"github.com/aholstenson/kvarn/internal/config/tomlstore"
)

// apiKeyEntry is the on-disk representation of a single key. The KeyID is the
// TOML table key, so it is not repeated in the entry. Expires is stored as an
// RFC3339 string rather than *time.Time because go-toml/v2 marshals a pointer
// time as a quoted string it then refuses to decode back.
type apiKeyEntry struct {
	Name     string    `toml:"name"`
	Hash     string    `toml:"hash"`
	Projects []string  `toml:"projects"`
	Created  time.Time `toml:"created"`
	Expires  string    `toml:"expires,omitempty"`
	Disabled bool      `toml:"disabled,omitempty"`
}

// fileData mirrors the [keyid] table-per-key TOML layout.
type fileData map[string]apiKeyEntry

// Store is a TOML file-backed API key store.
type Store struct {
	inner *tomlstore.Store[string, fileData, apiKeyEntry, *apikey.APIKey]
}

// New creates a Store backed by the given file path.
func New(path string) *Store {
	return &Store{inner: tomlstore.New(
		path,
		tomlstore.Secret,
		tomlstore.Schema[string, fileData, apiKeyEntry]{
			NewFileData: func() fileData { return fileData{} },
			Get: func(fd fileData, k string) (apiKeyEntry, bool) {
				e, ok := fd[k]
				return e, ok
			},
			Put: func(fd *fileData, k string, e apiKeyEntry) {
				if *fd == nil {
					*fd = fileData{}
				}
				(*fd)[k] = e
			},
			Delete: func(fd *fileData, k string) bool {
				if _, ok := (*fd)[k]; !ok {
					return false
				}
				delete(*fd, k)
				return true
			},
			Keys: func(fd fileData) []string {
				ks := make([]string, 0, len(fd))
				for k := range fd {
					ks = append(ks, k)
				}
				return ks
			},
			Less: func(a, b string) bool { return a < b },
		},
		entryToKey,
		keyToEntry,
	)}
}

// DefaultPath returns the default API key store path.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kvarn", "apikeys.toml")
}

// OpenDefault returns a Store backed by path, or by DefaultPath() when path is
// empty. It is the shared entry point for the "flag override → user default"
// behaviour.
func OpenDefault(path string) *Store {
	if path == "" {
		path = DefaultPath()
	}
	return New(path)
}

func entryToKey(keyID string, e apiKeyEntry) (*apikey.APIKey, error) {
	projects := make([]string, len(e.Projects))
	copy(projects, e.Projects)
	var expires *time.Time
	if e.Expires != "" {
		t, err := time.Parse(time.RFC3339, e.Expires)
		if err != nil {
			return nil, fmt.Errorf("parse expires for key %q: %w", keyID, err)
		}
		expires = &t
	}
	return &apikey.APIKey{
		KeyID:    keyID,
		Name:     e.Name,
		Hash:     e.Hash,
		Projects: projects,
		Created:  e.Created,
		Expires:  expires,
		Disabled: e.Disabled,
	}, nil
}

func keyToEntry(k *apikey.APIKey) (string, apiKeyEntry) {
	projects := make([]string, len(k.Projects))
	copy(projects, k.Projects)
	var expires string
	if k.Expires != nil {
		expires = k.Expires.UTC().Format(time.RFC3339)
	}
	return k.KeyID, apiKeyEntry{
		Name:     k.Name,
		Hash:     k.Hash,
		Projects: projects,
		Created:  k.Created,
		Expires:  expires,
		Disabled: k.Disabled,
	}
}

func (s *Store) Get(ctx context.Context, keyID string) (*apikey.APIKey, error) {
	return s.inner.Get(ctx, keyID)
}

func (s *Store) List(ctx context.Context) ([]*apikey.APIKey, error) {
	return s.inner.List(ctx)
}

func (s *Store) Put(ctx context.Context, k *apikey.APIKey) error {
	return s.inner.Put(ctx, k)
}

func (s *Store) Delete(ctx context.Context, keyID string) error {
	return s.inner.Delete(ctx, keyID)
}
