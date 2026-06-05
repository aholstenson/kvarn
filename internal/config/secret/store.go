package secret

import "context"

// Type identifies how a secret is delivered to a job. Adding new types
// requires changes in the orchestrator's resolution path and (for non-env
// types) the egress proxy.
const (
	// TypeEnv injects the value verbatim as an environment variable.
	TypeEnv = "env"
	// TypeBearer injects an unguessable placeholder as the env-var value;
	// the egress proxy substitutes the placeholder for the real value in
	// outbound request headers.
	TypeBearer = "bearer"
)

// Secret holds a per-project named secret with delivery semantics.
type Secret struct {
	Project string
	Name    string
	Type    string
	Value   string
}

// Store provides CRUD operations for project-scoped secrets. Get and Delete
// return tomlstore.ErrNotFound when no entry matches; List returns an empty,
// non-nil slice for a project that has no secrets.
type Store interface {
	Get(ctx context.Context, project, name string) (*Secret, error)
	List(ctx context.Context, project string) ([]*Secret, error)
	Put(ctx context.Context, s *Secret) error
	Delete(ctx context.Context, project, name string) error
}
