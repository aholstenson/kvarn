package tomlstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/aholstenson/kvarn/internal/config/atomicfile"
	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
	"github.com/pelletier/go-toml/v2"
)

type fileData struct {
	// Defaults is a pointer with omitempty so a config without a [defaults]
	// block round-trips through Put without gaining an empty table.
	Defaults *defaultsEntry         `toml:"defaults,omitempty"`
	Forges   map[string]*forgeEntry `toml:"forges"`
}

// defaultsEntry mirrors the forge-wide [defaults] block in forges.toml.
type defaultsEntry struct {
	BranchPrefix      string   `toml:"branch_prefix,omitempty"`
	Labels            []string `toml:"labels,omitempty"`
	CommitAuthorName  string   `toml:"commit_author_name,omitempty"`
	CommitAuthorEmail string   `toml:"commit_author_email,omitempty"`
}

type forgeEntry struct {
	Type              string   `toml:"type"`
	Credential        string   `toml:"credential,omitempty"`
	BranchPrefix      string   `toml:"branch_prefix,omitempty"`
	Labels            []string `toml:"labels,omitempty"`
	CommitAuthorName  string   `toml:"commit_author_name,omitempty"`
	CommitAuthorEmail string   `toml:"commit_author_email,omitempty"`
}

// Store is a TOML file-backed forge config store.
type Store struct {
	path string
	mu   sync.RWMutex
}

// New creates a Store backed by the given file path.
func New(path string) *Store {
	return &Store{path: path}
}

// DefaultPath returns the default forge config store path.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kvarn", "forges.toml")
}

func (s *Store) load() (*fileData, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &fileData{Forges: make(map[string]*forgeEntry)}, nil
		}
		return nil, err
	}

	var fd fileData
	if err := toml.Unmarshal(data, &fd); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	if fd.Forges == nil {
		fd.Forges = make(map[string]*forgeEntry)
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

	return atomicfile.Write(s.path, data, 0644)
}

func (s *Store) Get(_ context.Context, name string) (*forgeconfig.ForgeConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fd, err := s.load()
	if err != nil {
		return nil, err
	}

	entry, ok := fd.Forges[name]
	if !ok {
		return nil, fmt.Errorf("forge config %q not found", name)
	}

	return entryToConfig(name, entry), nil
}

func (s *Store) List(_ context.Context) ([]*forgeconfig.ForgeConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fd, err := s.load()
	if err != nil {
		return nil, err
	}

	var result []*forgeconfig.ForgeConfig
	for name, entry := range fd.Forges {
		result = append(result, entryToConfig(name, entry))
	}
	return result, nil
}

// Defaults returns the parsed [defaults] block. A missing block or file yields
// a zero-value Defaults with no error so callers can layer built-in fallbacks
// via forgeconfig.ResolveBehavior.
func (s *Store) Defaults(_ context.Context) (forgeconfig.Defaults, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fd, err := s.load()
	if err != nil {
		return forgeconfig.Defaults{}, err
	}
	if fd.Defaults == nil {
		return forgeconfig.Defaults{}, nil
	}

	labels := make([]string, len(fd.Defaults.Labels))
	copy(labels, fd.Defaults.Labels)
	return forgeconfig.Defaults{
		BranchPrefix:      fd.Defaults.BranchPrefix,
		CommitAuthorName:  fd.Defaults.CommitAuthorName,
		CommitAuthorEmail: fd.Defaults.CommitAuthorEmail,
		Labels:            labels,
	}, nil
}

func (s *Store) Put(_ context.Context, fc *forgeconfig.ForgeConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	fd, err := s.load()
	if err != nil {
		return err
	}

	fd.Forges[fc.Name] = &forgeEntry{
		Type:              fc.Type,
		Credential:        fc.Credential,
		BranchPrefix:      fc.BranchPrefix,
		Labels:            fc.Labels,
		CommitAuthorName:  fc.CommitAuthorName,
		CommitAuthorEmail: fc.CommitAuthorEmail,
	}

	return s.save(fd)
}

func (s *Store) Delete(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	fd, err := s.load()
	if err != nil {
		return err
	}

	if _, ok := fd.Forges[name]; !ok {
		return fmt.Errorf("forge config %q not found", name)
	}

	delete(fd.Forges, name)
	return s.save(fd)
}

func entryToConfig(name string, e *forgeEntry) *forgeconfig.ForgeConfig {
	labels := make([]string, len(e.Labels))
	copy(labels, e.Labels)
	return &forgeconfig.ForgeConfig{
		Name:              name,
		Type:              e.Type,
		Credential:        e.Credential,
		BranchPrefix:      e.BranchPrefix,
		Labels:            labels,
		CommitAuthorName:  e.CommitAuthorName,
		CommitAuthorEmail: e.CommitAuthorEmail,
	}
}
