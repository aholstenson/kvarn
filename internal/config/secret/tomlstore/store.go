package tomlstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/aholstenson/kvarn/internal/config/atomicfile"
	"github.com/aholstenson/kvarn/internal/config/secret"
	"github.com/pelletier/go-toml/v2"
)

// secretEntry is the on-disk representation of a single secret.
type secretEntry struct {
	Type  string `toml:"type"`
	Value string `toml:"value"`
}

// fileData mirrors the nested [project.name] TOML layout.
type fileData map[string]map[string]secretEntry

// Store is a TOML file-backed secret store. The file is enforced to mode
// 0600 on every write so secret material never leaks to other users.
type Store struct {
	path string
	mu   sync.RWMutex
}

// New creates a Store backed by the given file path.
func New(path string) *Store {
	return &Store{path: path}
}

// DefaultPath returns the default secrets store path.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kvarn", "secrets.toml")
}

// OpenDefault returns a Store backed by path, or by DefaultPath() when
// path is empty. It is the shared entry point for callers that want
// the standard "flag override → user default" behaviour.
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

func validateType(t string) error {
	switch t {
	case secret.TypeEnv, secret.TypeBearer:
		return nil
	}
	return fmt.Errorf("invalid secret type %q: must be %q or %q",
		t, secret.TypeEnv, secret.TypeBearer)
}

func (s *Store) Get(_ context.Context, project, name string) (*secret.Secret, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fd, err := s.load()
	if err != nil {
		return nil, err
	}

	proj, ok := fd[project]
	if !ok {
		return nil, fmt.Errorf("secret %q not found for project %q", name, project)
	}
	entry, ok := proj[name]
	if !ok {
		return nil, fmt.Errorf("secret %q not found for project %q", name, project)
	}

	return &secret.Secret{
		Project: project,
		Name:    name,
		Type:    entry.Type,
		Value:   entry.Value,
	}, nil
}

func (s *Store) List(_ context.Context, project string) ([]*secret.Secret, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fd, err := s.load()
	if err != nil {
		return nil, err
	}

	proj, ok := fd[project]
	if !ok {
		return nil, nil
	}

	names := make([]string, 0, len(proj))
	for name := range proj {
		names = append(names, name)
	}
	sort.Strings(names)

	result := make([]*secret.Secret, 0, len(proj))
	for _, name := range names {
		entry := proj[name]
		result = append(result, &secret.Secret{
			Project: project,
			Name:    name,
			Type:    entry.Type,
			Value:   entry.Value,
		})
	}
	return result, nil
}

func (s *Store) Put(_ context.Context, sec *secret.Secret) error {
	if err := validateType(sec.Type); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	fd, err := s.load()
	if err != nil {
		return err
	}

	proj, ok := fd[sec.Project]
	if !ok {
		proj = make(map[string]secretEntry)
		fd[sec.Project] = proj
	}
	proj[sec.Name] = secretEntry{Type: sec.Type, Value: sec.Value}

	return s.save(fd)
}

func (s *Store) Delete(_ context.Context, project, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	fd, err := s.load()
	if err != nil {
		return err
	}

	proj, ok := fd[project]
	if !ok {
		return fmt.Errorf("secret %q not found for project %q", name, project)
	}
	if _, ok := proj[name]; !ok {
		return fmt.Errorf("secret %q not found for project %q", name, project)
	}
	delete(proj, name)
	if len(proj) == 0 {
		delete(fd, project)
	}
	return s.save(fd)
}
