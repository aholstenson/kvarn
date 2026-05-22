package tomlstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/aholstenson/kvarn/internal/config/apikey"
	"github.com/aholstenson/kvarn/internal/config/atomicfile"
	"github.com/pelletier/go-toml/v2"
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

// Store is a TOML file-backed API key store. The file is enforced to mode 0600
// on every write so key hashes never leak to other users. Each call re-reads
// the file, so changes made by `kvarn key create` are picked up without a
// restart; writes are atomic so a concurrent read never sees a partial file.
type Store struct {
	path string
	mu   sync.RWMutex
}

// New creates a Store backed by the given file path.
func New(path string) *Store {
	return &Store{path: path}
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

func (s *Store) load() (fileData, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return fileData{}, nil
		}
		return nil, err
	}

	var fd fileData
	if err := toml.Unmarshal(data, &fd); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	if fd == nil {
		fd = fileData{}
	}
	return fd, nil
}

func (s *Store) save(fd fileData) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}

	data, err := toml.Marshal(fd)
	if err != nil {
		return err
	}

	return atomicfile.Write(s.path, data, 0o600)
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

func (s *Store) Get(_ context.Context, keyID string) (*apikey.APIKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fd, err := s.load()
	if err != nil {
		return nil, err
	}

	entry, ok := fd[keyID]
	if !ok {
		return nil, apikey.ErrNotFound
	}
	return entryToKey(keyID, entry)
}

func (s *Store) List(_ context.Context) ([]*apikey.APIKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fd, err := s.load()
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(fd))
	for id := range fd {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	result := make([]*apikey.APIKey, 0, len(fd))
	for _, id := range ids {
		k, err := entryToKey(id, fd[id])
		if err != nil {
			return nil, err
		}
		result = append(result, k)
	}
	return result, nil
}

func (s *Store) Put(_ context.Context, k *apikey.APIKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	fd, err := s.load()
	if err != nil {
		return err
	}

	projects := make([]string, len(k.Projects))
	copy(projects, k.Projects)
	var expires string
	if k.Expires != nil {
		expires = k.Expires.UTC().Format(time.RFC3339)
	}
	fd[k.KeyID] = apiKeyEntry{
		Name:     k.Name,
		Hash:     k.Hash,
		Projects: projects,
		Created:  k.Created,
		Expires:  expires,
		Disabled: k.Disabled,
	}

	return s.save(fd)
}

func (s *Store) Delete(_ context.Context, keyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	fd, err := s.load()
	if err != nil {
		return err
	}

	if _, ok := fd[keyID]; !ok {
		return apikey.ErrNotFound
	}
	delete(fd, keyID)
	return s.save(fd)
}
