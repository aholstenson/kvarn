package tomlstore

import (
	"context"
	"os"
	"path/filepath"

	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
	"github.com/aholstenson/kvarn/internal/config/tomlstore"
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
	inner *tomlstore.Store[string, fileData, *forgeEntry, *forgeconfig.ForgeConfig]
}

// New creates a Store backed by the given file path.
func New(path string) *Store {
	return &Store{inner: tomlstore.New(
		path,
		tomlstore.Config,
		tomlstore.Schema[string, fileData, *forgeEntry]{
			NewFileData: func() fileData {
				return fileData{Forges: map[string]*forgeEntry{}}
			},
			Get: func(fd fileData, k string) (*forgeEntry, bool) {
				e, ok := fd.Forges[k]
				return e, ok
			},
			Put: func(fd *fileData, k string, e *forgeEntry) {
				if fd.Forges == nil {
					fd.Forges = map[string]*forgeEntry{}
				}
				fd.Forges[k] = e
			},
			Delete: func(fd *fileData, k string) bool {
				if _, ok := fd.Forges[k]; !ok {
					return false
				}
				delete(fd.Forges, k)
				return true
			},
			Keys: func(fd fileData) []string {
				ks := make([]string, 0, len(fd.Forges))
				for k := range fd.Forges {
					ks = append(ks, k)
				}
				return ks
			},
			Less: func(a, b string) bool { return a < b },
		},
		entryToConfig,
		configToEntry,
	)}
}

// DefaultPath returns the default forge config store path.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kvarn", "forges.toml")
}

func entryToConfig(name string, e *forgeEntry) (*forgeconfig.ForgeConfig, error) {
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
	}, nil
}

func configToEntry(fc *forgeconfig.ForgeConfig) (string, *forgeEntry) {
	labels := make([]string, len(fc.Labels))
	copy(labels, fc.Labels)
	return fc.Name, &forgeEntry{
		Type:              fc.Type,
		Credential:        fc.Credential,
		BranchPrefix:      fc.BranchPrefix,
		Labels:            labels,
		CommitAuthorName:  fc.CommitAuthorName,
		CommitAuthorEmail: fc.CommitAuthorEmail,
	}
}

func (s *Store) Get(ctx context.Context, name string) (*forgeconfig.ForgeConfig, error) {
	return s.inner.Get(ctx, name)
}

func (s *Store) List(ctx context.Context) ([]*forgeconfig.ForgeConfig, error) {
	return s.inner.List(ctx)
}

func (s *Store) Put(ctx context.Context, fc *forgeconfig.ForgeConfig) error {
	return s.inner.Put(ctx, fc)
}

func (s *Store) Delete(ctx context.Context, name string) error {
	return s.inner.Delete(ctx, name)
}

// Defaults returns the parsed [defaults] block. A missing block yields a
// zero-value Defaults so callers can layer built-in fallbacks via
// forgeconfig.ResolveBehavior. A malformed file surfaces as an error.
func (s *Store) Defaults(ctx context.Context) (forgeconfig.Defaults, error) {
	fd, err := s.inner.Load(ctx)
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
