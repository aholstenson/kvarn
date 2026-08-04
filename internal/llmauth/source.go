// Package llmauth adapts kvarn's stored LLM provider credentials to the
// credential source llms-go resolves model authentication through.
package llmauth

import (
	"context"
	"fmt"
	"net/http"

	llms "github.com/aholstenson/llms-go"

	"github.com/aholstenson/kvarn/internal/config/credential"
)

// Source resolves LLM provider credentials for llms-go, preferring the [llm]
// block of credentials.toml and falling back to the environment for any
// provider the store does not cover. That ordering lets an operator keep
// long-lived keys in the file while a one-off `ANTHROPIC_API_KEY=... kvarn
// run` still works, and it leaves an env-only setup untouched.
//
// llms-go resolves a credential per outbound request, and the store is re-read
// on each one — the same stance the rest of the host config takes, and what
// makes editing the file enough to rotate a key under a running orchestrator.
// A read is a file read and a parse with no lock held, which is nothing beside
// the model call it authenticates.
type Source struct {
	store    credential.LLMStore
	fallback llms.CredentialSource
}

// SourceOption configures a Source during construction.
type SourceOption func(*Source)

// WithFallback replaces the credential source consulted for providers absent
// from the store. Defaults to llms.EnvCredentials.
func WithFallback(fallback llms.CredentialSource) SourceOption {
	return func(s *Source) { s.fallback = fallback }
}

// NewSource returns a CredentialSource backed by store.
func NewSource(store credential.LLMStore, opts ...SourceOption) *Source {
	s := &Source{
		store:    store,
		fallback: &llms.EnvCredentials{},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Credential implements llms.CredentialSource.
//
// A store read failure is surfaced rather than treated as "no credentials
// configured": a malformed file must not silently demote every provider to
// whatever happens to be in the environment.
func (s *Source) Credential(ctx context.Context, provider string) (llms.Credential, error) {
	creds, err := s.store.List(ctx)
	if err != nil {
		return llms.Credential{}, fmt.Errorf("read LLM credentials: %w", err)
	}

	for _, c := range creds {
		if c.Provider != provider {
			continue
		}
		// An entry carrying neither a key nor headers cannot authenticate
		// anything. Treating it as absent lets the environment answer instead
		// of the provider rejecting an empty credential at request time.
		if c.APIKey == "" && len(c.Headers) == 0 {
			break
		}
		cred := llms.Credential{APIKey: c.APIKey}
		if len(c.Headers) > 0 {
			cred.Headers = make(http.Header, len(c.Headers))
			for name, value := range c.Headers {
				cred.Headers.Set(name, value)
			}
		}
		return cred, nil
	}

	return s.fallback.Credential(ctx, provider)
}
