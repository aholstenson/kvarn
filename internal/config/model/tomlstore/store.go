package tomlstore

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/cockroachdb/errors"
	"github.com/pelletier/go-toml/v2"
)

// fileData mirrors the on-disk layout:
//
//	[models]
//	coding-agent = "anthropic/claude-sonnet-4-6"
//	coding-agent-small = "anthropic/claude-haiku-4-5"
type fileData struct {
	Models map[string]string `toml:"models"`
}

// Store is a TOML file-backed model-alias override store.
type Store struct {
	path string
	mu   sync.RWMutex
}

// New creates a Store backed by the given file path.
func New(path string) *Store {
	return &Store{path: path}
}

// DefaultPath returns the default models store path.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kvarn", "models.toml")
}

// OpenDefault returns a Store backed by path, or by DefaultPath() when path
// is empty.
func OpenDefault(path string) *Store {
	if path == "" {
		path = DefaultPath()
	}
	return New(path)
}

func (s *Store) All(_ context.Context) (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}

	var fd fileData
	if err := toml.Unmarshal(data, &fd); err != nil {
		return nil, errors.Wrapf(err, "parse %s", s.path)
	}
	if fd.Models == nil {
		return map[string]string{}, nil
	}
	return fd.Models, nil
}
