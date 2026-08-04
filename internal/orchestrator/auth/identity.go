package auth

import (
	"context"

	"github.com/aholstenson/kvarn/internal/config/apikey"
)

// Source names how a caller proved who they are. It is recorded on every audit
// line because nothing else tells the two apart: a local caller presents no
// key, so without this "who stopped the host" has no answer.
const (
	// SourceAPIKey is a bearer token validated against the API key store.
	SourceAPIKey = "api_key"
	// SourceLocal is the host-local control socket, where the filesystem
	// rather than a secret decides who may connect.
	SourceLocal = "local"
)

// Identity is the authenticated caller. It is attached to the request context
// by whichever interceptor authenticated the request and read by the handler
// authorization checks, which never learn which listener the request arrived
// on — one authorization model, two ways of satisfying it.
type Identity struct {
	// Source is one of the Source* constants above.
	Source string
	// KeyID and KeyName identify the caller within Source. For an API key they
	// are the key's own; for a local caller KeyID is empty — there is no key to
	// attribute per-key resource limits to — and KeyName is a fixed label, with
	// the calling process described by Peer.
	KeyID   string
	KeyName string
	// Peer describes the process behind a SourceLocal identity, for the audit
	// log. Empty for an API key.
	Peer string
	// Projects is the project scope; apikey.Wildcard covers every project.
	Projects []string
	// Capabilities are the host-level actions this caller may take.
	Capabilities []apikey.Capability
}

// AllowsProject reports whether the identity is scoped to the named project,
// either explicitly or via the wildcard entry.
func (id *Identity) AllowsProject(name string) bool {
	for _, p := range id.Projects {
		if p == apikey.Wildcard || p == name {
			return true
		}
	}
	return false
}

// IsWildcard reports whether the identity's project scope is unbounded. It is
// the difference between a request that reaches the caller's own work and one
// that reaches every job on the host, which is what decides whether an
// unfiltered bulk operation needs a capability on top of its project scope.
func (id *Identity) IsWildcard() bool {
	for _, p := range id.Projects {
		if p == apikey.Wildcard {
			return true
		}
	}
	return false
}

// Allows reports whether the identity holds the given capability.
func (id *Identity) Allows(c apikey.Capability) bool {
	for _, have := range id.Capabilities {
		if have == c {
			return true
		}
	}
	return false
}

type identityKey struct{}

// WithIdentity returns a context carrying id.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFrom returns the identity attached to ctx, if any.
func IdentityFrom(ctx context.Context) (*Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(*Identity)
	return id, ok
}
