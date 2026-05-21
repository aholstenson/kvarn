package tomlstore

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/cockroachdb/errors"
	"github.com/pelletier/go-toml/v2"

	modelcfg "github.com/aholstenson/kvarn/internal/config/model"
)

// entryData mirrors a single [models.<alias>] block in agents.toml.
type entryData struct {
	Model           string `toml:"model"`
	ThinkingTokens  *int   `toml:"thinking_tokens"`
	MaxOutputTokens *int   `toml:"max_output_tokens"`
}

// fileData mirrors the on-disk layout:
//
//	[models.coding-agent]
//	model            = "anthropic/claude-sonnet-4-6"
//	thinking_tokens  = 8000
//	max_output_tokens = 16384
type fileData struct {
	Models map[string]entryData `toml:"models"`
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

// DefaultPath returns the default agents config path.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kvarn", "agents.toml")
}

// OpenDefault returns a Store backed by path, or by DefaultPath() when path
// is empty.
func OpenDefault(path string) *Store {
	if path == "" {
		path = DefaultPath()
	}
	return New(path)
}

func (s *Store) All(_ context.Context) (map[string]modelcfg.RawEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]modelcfg.RawEntry{}, nil
		}
		return nil, err
	}

	var fd fileData
	if err := toml.Unmarshal(data, &fd); err != nil {
		return nil, errors.Wrapf(err, "parse %s", s.path)
	}
	if fd.Models == nil {
		return map[string]modelcfg.RawEntry{}, nil
	}

	out := make(map[string]modelcfg.RawEntry, len(fd.Models))
	for alias, e := range fd.Models {
		out[alias] = modelcfg.RawEntry{
			ModelID:         e.Model,
			ThinkingTokens:  e.ThinkingTokens,
			MaxOutputTokens: e.MaxOutputTokens,
		}
	}
	return out, nil
}
