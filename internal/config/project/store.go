package project

import "context"

// JobLimits is the per-job-mode override block for a project. Today it only
// carries a max-cost cap, but the shape is designed to take per-mode model
// selection without breaking existing config files when that lands.
type JobLimits struct {
	MaxCostUSD *float64
}

// Project represents a configured project with its repository details.
type Project struct {
	Name          string
	RepoURL       string // shorthand like "org/repo" or full URL
	DefaultBranch string
	Forge         string // references forge config by name
	// MaxCostUSD overrides the user-level default cost cap for this project.
	// Nil means "inherit from defaults". Resolution order is documented on
	// internal/config/limits.
	MaxCostUSD *float64
	// ReportCostOnPR overrides whether the work-log PR comment includes a
	// cost section. Nil means "inherit from defaults".
	ReportCostOnPR *bool
	// Jobs holds per-job-mode overrides keyed by mode name (auto, implement,
	// fix, review, research). nil/empty means no per-mode overrides.
	Jobs map[string]JobLimits
}

// Store provides CRUD operations for projects.
type Store interface {
	Get(ctx context.Context, name string) (*Project, error)
	List(ctx context.Context) ([]*Project, error)
	Put(ctx context.Context, p *Project) error
	Delete(ctx context.Context, name string) error
}
