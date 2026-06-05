package tomlstore

import (
	"context"
	"os"
	"path/filepath"

	"github.com/aholstenson/kvarn/internal/config/credential"
	"github.com/aholstenson/kvarn/internal/config/tomlstore"
)

type fileData struct {
	Credentials map[string]map[string]string `toml:"credentials"`
}

// Store is a TOML file-backed credential store.
type Store struct {
	inner *tomlstore.Store[string, fileData, map[string]string, *credential.Credential]
}

// New creates a Store backed by the given file path.
func New(path string) *Store {
	return &Store{inner: tomlstore.New(
		path,
		tomlstore.Secret,
		tomlstore.Schema[string, fileData, map[string]string]{
			NewFileData: func() fileData {
				return fileData{Credentials: map[string]map[string]string{}}
			},
			Get: func(fd fileData, k string) (map[string]string, bool) {
				e, ok := fd.Credentials[k]
				return e, ok
			},
			Put: func(fd *fileData, k string, e map[string]string) {
				if fd.Credentials == nil {
					fd.Credentials = map[string]map[string]string{}
				}
				fd.Credentials[k] = e
			},
			Delete: func(fd *fileData, k string) bool {
				if _, ok := fd.Credentials[k]; !ok {
					return false
				}
				delete(fd.Credentials, k)
				return true
			},
			Keys: func(fd fileData) []string {
				ks := make([]string, 0, len(fd.Credentials))
				for k := range fd.Credentials {
					ks = append(ks, k)
				}
				return ks
			},
			Less: func(a, b string) bool { return a < b },
		},
		entryToCredential,
		credentialToEntry,
	)}
}

// DefaultPath returns the default credential store path.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kvarn", "credentials.toml")
}

func entryToCredential(name string, entry map[string]string) (*credential.Credential, error) {
	config := make(map[string]string, len(entry))
	for k, v := range entry {
		config[k] = v
	}
	return &credential.Credential{
		Name:   name,
		Config: config,
	}, nil
}

func credentialToEntry(c *credential.Credential) (string, map[string]string) {
	config := make(map[string]string, len(c.Config))
	for k, v := range c.Config {
		config[k] = v
	}
	return c.Name, config
}

func (s *Store) Get(ctx context.Context, name string) (*credential.Credential, error) {
	return s.inner.Get(ctx, name)
}

func (s *Store) List(ctx context.Context) ([]*credential.Credential, error) {
	return s.inner.List(ctx)
}

func (s *Store) Put(ctx context.Context, c *credential.Credential) error {
	return s.inner.Put(ctx, c)
}

func (s *Store) Delete(ctx context.Context, name string) error {
	return s.inner.Delete(ctx, name)
}
