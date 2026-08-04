package credential

import "context"

// Credential holds authentication details as an opaque key-value map.
// The interpretation of the config entries depends on the forge type.
type Credential struct {
	Name   string
	Config map[string]string
}

// Store provides CRUD operations for credentials. Get and Delete return
// tomlstore.ErrNotFound when no entry matches.
type Store interface {
	Get(ctx context.Context, name string) (*Credential, error)
	List(ctx context.Context) ([]*Credential, error)
	Put(ctx context.Context, c *Credential) error
	Delete(ctx context.Context, name string) error
}

// LLMCredential is the authentication material for a single LLM provider.
// Provider is the name llms-go resolves models under — the segment before the
// slash in a qualified model ID such as "anthropic/claude-sonnet-4-6", so one
// of anthropic, openai, openrouter or google. A credential stored under any
// other name is never consulted.
//
// This is kept apart from Credential because the two are addressed
// differently: a forge credential carries a name the operator chooses and
// forge config points at, while an LLM credential is looked up under a
// provider name that llms-go fixes. One shared namespace would let a forge
// credential name collide with a reserved provider name.
//
// APIKey lands in the provider's native auth header. Headers are applied on
// top and carry whatever else the endpoint needs — a gateway token, a tenant
// ID. A credential with only Headers set is valid: it means authentication is
// carried entirely by those headers.
type LLMCredential struct {
	Provider string
	APIKey   string
	Headers  map[string]string
}

// LLMStore provides CRUD operations for LLM provider credentials, keyed by
// provider name. Get and Delete return tomlstore.ErrNotFound when no entry
// matches.
type LLMStore interface {
	Get(ctx context.Context, provider string) (*LLMCredential, error)
	List(ctx context.Context) ([]*LLMCredential, error)
	Put(ctx context.Context, c *LLMCredential) error
	Delete(ctx context.Context, provider string) error
}
