package secret

import "context"

// Type identifies whether a secret's real value enters the VM. Adding new
// types requires changes in the orchestrator's resolution path and (for
// non-env types) the egress proxy. It describes the delivery boundary only;
// how a managed secret is applied (bearer/basic/oauth) is a separate concern
// expressed per usage site in kvarn.yml.
const (
	// TypeEnv injects the value verbatim as an environment variable; the real
	// value lives inside the VM.
	TypeEnv = "env"
	// TypeManaged keeps the real value on the host: the VM only ever sees an
	// unguessable placeholder, and the egress proxy substitutes the real value
	// into matching outbound requests just before they leave the host.
	TypeManaged = "managed"
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
