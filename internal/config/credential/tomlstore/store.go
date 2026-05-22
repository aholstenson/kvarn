package tomlstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/aholstenson/kvarn/internal/config/atomicfile"
	"github.com/aholstenson/kvarn/internal/config/credential"
	"github.com/pelletier/go-toml/v2"
)

type fileData struct {
	Credentials map[string]map[string]string `toml:"credentials"`
}

// Store is a TOML file-backed credential store.
type Store struct {
	path string
	mu   sync.RWMutex
}

// New creates a Store backed by the given file path.
func New(path string) *Store {
	return &Store{path: path}
}

// DefaultPath returns the default credential store path.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kvarn", "credentials.toml")
}

func (s *Store) load() (*fileData, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &fileData{Credentials: make(map[string]map[string]string)}, nil
		}
		return nil, err
	}

	var fd fileData
	if err := toml.Unmarshal(data, &fd); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	if fd.Credentials == nil {
		fd.Credentials = make(map[string]map[string]string)
	}
	return &fd, nil
}

func (s *Store) save(fd *fileData) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}

	data, err := toml.Marshal(fd)
	if err != nil {
		return err
	}

	return atomicfile.Write(s.path, data, 0600)
}

func (s *Store) Get(_ context.Context, name string) (*credential.Credential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fd, err := s.load()
	if err != nil {
		return nil, err
	}

	entry, ok := fd.Credentials[name]
	if !ok {
		return nil, fmt.Errorf("credential %q not found", name)
	}

	config := make(map[string]string, len(entry))
	for k, v := range entry {
		config[k] = v
	}

	return &credential.Credential{
		Name:   name,
		Config: config,
	}, nil
}

func (s *Store) List(_ context.Context) ([]*credential.Credential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fd, err := s.load()
	if err != nil {
		return nil, err
	}

	var result []*credential.Credential
	for name, entry := range fd.Credentials {
		config := make(map[string]string, len(entry))
		for k, v := range entry {
			config[k] = v
		}
		result = append(result, &credential.Credential{
			Name:   name,
			Config: config,
		})
	}
	return result, nil
}

func (s *Store) Put(_ context.Context, c *credential.Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	fd, err := s.load()
	if err != nil {
		return err
	}

	config := make(map[string]string, len(c.Config))
	for k, v := range c.Config {
		config[k] = v
	}
	fd.Credentials[c.Name] = config

	return s.save(fd)
}

func (s *Store) Delete(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	fd, err := s.load()
	if err != nil {
		return err
	}

	if _, ok := fd.Credentials[name]; !ok {
		return fmt.Errorf("credential %q not found", name)
	}

	delete(fd.Credentials, name)
	return s.save(fd)
}
