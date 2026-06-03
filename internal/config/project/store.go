package project

import "context"

// JobLimits is the per-job-mode override block for a project. Today it only
// carries a max-cost cap, but the shape is designed to take per-mode model
// selection without breaking existing config files when that lands.
type JobLimits struct {
	MaxCostUSD           *float64
	MaxValidationRetries *int
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
	// MaxValidationRetries overrides the user-level default for how many
	// additional agent attempts to allow after a required validation step
	// fails. Nil means "inherit from defaults". 0 means "no retries".
	MaxValidationRetries *int
	// Jobs holds per-job-mode overrides keyed by mode name (auto, implement,
	// fix, review, research). nil/empty means no per-mode overrides.
	Jobs map[string]JobLimits
	// The following override the selected forge's PR behavior for this project.
	// Empty/zero means "inherit from the forge, then the global [defaults], then
	// the compiled-in constants"; see forge.ForgeConfig.ResolveBehavior. They
	// live here, not on the forge, because one forge is shared by many projects
	// and these settings vary per repository (different repos use different label
	// sets and branch conventions).
	BranchPrefix      string
	Labels            []string
	CommitAuthorName  string
	CommitAuthorEmail string
	// CloneDepth overrides the default shallow-clone depth. Nil inherits
	// scm.DefaultCloneDepth. A positive value caps history to that many
	// commits; 0 means a full clone (use for projects whose tooling needs
	// complete history, e.g. version inference from tags).
	CloneDepth *int
}

// Store provides CRUD operations for projects.
type Store interface {
	Get(ctx context.Context, name string) (*Project, error)
	List(ctx context.Context) ([]*Project, error)
	Put(ctx context.Context, p *Project) error
	Delete(ctx context.Context, name string) error
}
