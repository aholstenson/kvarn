package auth

import "context"

// Identity is the authenticated caller derived from a valid API key. It is
// attached to the request context by the interceptor and read by the handler
// authorization checks.
type Identity struct {
	KeyID    string
	KeyName  string
	Projects []string
}

// AllowsProject reports whether the identity is scoped to the named project,
// either explicitly or via the "*" wildcard entry.
func (id *Identity) AllowsProject(name string) bool {
	for _, p := range id.Projects {
		if p == "*" || p == name {
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
