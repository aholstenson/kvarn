package tomlstore

import (
	"context"
	"os"
	"path/filepath"

	"github.com/aholstenson/kvarn/internal/config/credential"
	"github.com/aholstenson/kvarn/internal/config/tomlstore"
)

// llmEntry is the on-disk representation of one LLM provider's credentials.
// The provider name is the TOML table key, so it is not repeated here.
type llmEntry struct {
	APIKey  string            `toml:"api_key,omitempty"`
	Headers map[string]string `toml:"headers,omitempty"`
}

// fileData mirrors the on-disk layout. Forge credentials and LLM provider
// credentials share the file — both are host secrets an operator manages in
// one place — but sit in separate blocks because they are addressed
// differently; see credential.LLMCredential.
//
//	[credentials.github-work]
//	token = "ghp_..."
//
//	[llm.anthropic]
//	api_key = "sk-ant-..."
//
//	[llm.openrouter]
//	api_key = "sk-or-..."
//	headers = { x-title = "kvarn" }
type fileData struct {
	Credentials map[string]map[string]string `toml:"credentials"`
	LLM         map[string]llmEntry          `toml:"llm,omitempty"`
}

// Store is a TOML file-backed credential store. The forge and LLM blocks are
// served by two generic stores over the same file: each loads, mutates its own
// block and writes the whole file back, so a write to one preserves the other.
// The generic store runs that load → mutate → save under a cross-process flock
// on the file, which is what keeps two writers from losing each other's edit —
// here between the two blocks as much as between two processes.
type Store struct {
	inner *tomlstore.Store[string, fileData, map[string]string, *credential.Credential]
	llm   *tomlstore.Store[string, fileData, llmEntry, *credential.LLMCredential]
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
	), llm: tomlstore.New(
		path,
		tomlstore.Secret,
		tomlstore.Schema[string, fileData, llmEntry]{
			NewFileData: func() fileData {
				return fileData{Credentials: map[string]map[string]string{}}
			},
			Get: func(fd fileData, k string) (llmEntry, bool) {
				e, ok := fd.LLM[k]
				return e, ok
			},
			Put: func(fd *fileData, k string, e llmEntry) {
				if fd.LLM == nil {
					fd.LLM = map[string]llmEntry{}
				}
				fd.LLM[k] = e
			},
			Delete: func(fd *fileData, k string) bool {
				if _, ok := fd.LLM[k]; !ok {
					return false
				}
				delete(fd.LLM, k)
				return true
			},
			Keys: func(fd fileData) []string {
				ks := make([]string, 0, len(fd.LLM))
				for k := range fd.LLM {
					ks = append(ks, k)
				}
				return ks
			},
			Less: func(a, b string) bool { return a < b },
		},
		entryToLLMCredential,
		llmCredentialToEntry,
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

// OpenDefault returns a Store backed by path, or by DefaultPath() when path
// is empty. It is the shared entry point for the "flag override → user
// default" behaviour.
func OpenDefault(path string) *Store {
	if path == "" {
		path = DefaultPath()
	}
	return New(path)
}

// Path returns the backing file path.
func (s *Store) Path() string { return s.inner.Path() }

func entryToLLMCredential(provider string, e llmEntry) (*credential.LLMCredential, error) {
	return &credential.LLMCredential{
		Provider: provider,
		APIKey:   e.APIKey,
		Headers:  copyHeaders(e.Headers),
	}, nil
}

func llmCredentialToEntry(c *credential.LLMCredential) (string, llmEntry) {
	return c.Provider, llmEntry{APIKey: c.APIKey, Headers: copyHeaders(c.Headers)}
}

func copyHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// LLM returns the view of this store that serves the [llm] block: the LLM
// provider credentials, keyed by provider name.
func (s *Store) LLM() credential.LLMStore { return (*llmView)(s) }

// llmView adapts a Store to credential.LLMStore. It is a distinct type so the
// two blocks' identically-named methods can coexist on one file-backed store.
type llmView Store

func (v *llmView) Get(ctx context.Context, provider string) (*credential.LLMCredential, error) {
	return v.llm.Get(ctx, provider)
}

func (v *llmView) List(ctx context.Context) ([]*credential.LLMCredential, error) {
	return v.llm.List(ctx)
}

func (v *llmView) Put(ctx context.Context, c *credential.LLMCredential) error {
	return v.llm.Put(ctx, c)
}

func (v *llmView) Delete(ctx context.Context, provider string) error {
	return v.llm.Delete(ctx, provider)
}
