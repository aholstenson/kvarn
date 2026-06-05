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
