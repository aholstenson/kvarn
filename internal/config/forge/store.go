package forge

import "context"

// ForgeConfig holds the configuration for a named forge instance.
type ForgeConfig struct {
	Name              string
	Type              string   // "github", "git"
	Credential        string   // references credential by name
	BranchPrefix      string   // default: "kvarn"
	Labels            []string
	CommitAuthorName  string   // default: "kvarn"
	CommitAuthorEmail string   // default: "kvarn@noreply"
}

// Store provides CRUD operations for forge configurations.
type Store interface {
	Get(ctx context.Context, name string) (*ForgeConfig, error)
	List(ctx context.Context) ([]*ForgeConfig, error)
	Put(ctx context.Context, fc *ForgeConfig) error
	Delete(ctx context.Context, name string) error
}
