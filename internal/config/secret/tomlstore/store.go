package tomlstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aholstenson/kvarn/internal/config/secret"
	"github.com/aholstenson/kvarn/internal/config/tomlstore"
)

// secretEntry is the on-disk representation of a single secret.
type secretEntry struct {
	Type  string `toml:"type"`
	Value string `toml:"value"`
}

// fileData mirrors the nested [project.name] TOML layout.
type fileData map[string]map[string]secretEntry

// secretKey is the composite lookup key for the generic store: a secret is
// addressed by (project, name).
type secretKey struct {
	Project string
	Name    string
}

// Store is a TOML file-backed secret store.
type Store struct {
	inner *tomlstore.Store[secretKey, fileData, secretEntry, *secret.Secret]
}

// New creates a Store backed by the given file path.
func New(path string) *Store {
	return &Store{inner: tomlstore.New(
		path,
		tomlstore.Secret,
		tomlstore.Schema[secretKey, fileData, secretEntry]{
			NewFileData: func() fileData { return fileData{} },
			Get: func(fd fileData, k secretKey) (secretEntry, bool) {
				proj, ok := fd[k.Project]
				if !ok {
					return secretEntry{}, false
				}
				e, ok := proj[k.Name]
				return e, ok
			},
			Put: func(fd *fileData, k secretKey, e secretEntry) {
				if *fd == nil {
					*fd = fileData{}
				}
				proj, ok := (*fd)[k.Project]
				if !ok {
					proj = map[string]secretEntry{}
					(*fd)[k.Project] = proj
				}
				proj[k.Name] = e
			},
			Delete: func(fd *fileData, k secretKey) bool {
				proj, ok := (*fd)[k.Project]
				if !ok {
					return false
				}
				if _, ok := proj[k.Name]; !ok {
					return false
				}
				delete(proj, k.Name)
				// Drop the project map when it goes empty so the on-disk
				// file does not retain stray empty tables.
				if len(proj) == 0 {
					delete(*fd, k.Project)
				}
				return true
			},
			Keys: func(fd fileData) []secretKey {
				n := 0
				for _, proj := range fd {
					n += len(proj)
				}
				ks := make([]secretKey, 0, n)
				for project, proj := range fd {
					for name := range proj {
						ks = append(ks, secretKey{Project: project, Name: name})
					}
				}
				return ks
			},
			Less: func(a, b secretKey) bool {
				if a.Project != b.Project {
					return a.Project < b.Project
				}
				return a.Name < b.Name
			},
		},
		func(k secretKey, e secretEntry) (*secret.Secret, error) {
			return &secret.Secret{
				Project: k.Project,
				Name:    k.Name,
				Type:    e.Type,
				Value:   e.Value,
			}, nil
		},
		func(s *secret.Secret) (secretKey, secretEntry) {
			return secretKey{Project: s.Project, Name: s.Name},
				secretEntry{Type: s.Type, Value: s.Value}
		},
	)}
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

func validateType(t string) error {
	switch t {
	case secret.TypeEnv, secret.TypeManaged:
		return nil
	}
	return fmt.Errorf("invalid secret type %q: must be %q or %q",
		t, secret.TypeEnv, secret.TypeManaged)
}

func (s *Store) Get(ctx context.Context, project, name string) (*secret.Secret, error) {
	return s.inner.Get(ctx, secretKey{Project: project, Name: name})
}

// List returns every secret in project sorted by name. A missing project
// yields an empty, non-nil slice rather than nil.
func (s *Store) List(ctx context.Context, project string) ([]*secret.Secret, error) {
	all, err := s.inner.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*secret.Secret, 0, len(all))
	for _, sec := range all {
		if sec.Project == project {
			out = append(out, sec)
		}
	}
	return out, nil
}

func (s *Store) Put(ctx context.Context, sec *secret.Secret) error {
	if err := validateType(sec.Type); err != nil {
		return err
	}
	return s.inner.Put(ctx, sec)
}

func (s *Store) Delete(ctx context.Context, project, name string) error {
	return s.inner.Delete(ctx, secretKey{Project: project, Name: name})
}
