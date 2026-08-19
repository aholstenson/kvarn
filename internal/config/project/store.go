package project

import (
	"context"

	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
)

// JobLimits is the per-job-mode override block for a project. Today it only
// carries a max-cost cap, but the shape is designed to take per-mode model
// selection without breaking existing config files when that lands.
type JobLimits struct {
	MaxCostUSD           *float64
	MaxValidationRetries *int
	// Priority overrides the project's scheduling priority for this job mode.
	// Nil inherits the project's own value.
	Priority *int
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
	// ReportCostOnPR and ReportWorklogOnPR are the superseded top-level
	// spellings of the same settings in PullRequest. They are still read so
	// existing configs keep working, and the block below wins when both are
	// present; `kvarn` warns when it takes a value from here.
	ReportCostOnPR    *bool
	ReportWorklogOnPR *bool
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
	// Preview is the `[projects.<name>.preview]` block: whether this project may
	// have preview environments, and under which domain.
	Preview Preview
	// PullRequest is the `[projects.<name>.pull_request]` block: what the pull
	// requests and comments this project's jobs produce should say. It is the
	// most specific operator layer, above the forge and the global defaults.
	PullRequest forgeconfig.PRContent
	// CloneDepth overrides the default shallow-clone depth. Nil inherits
	// scm.DefaultCloneDepth. A positive value caps history to that many
	// commits; 0 means a full clone (use for projects whose tooling needs
	// complete history, e.g. version inference from tags).
	CloneDepth *int
	// MirrorDepth overrides how much history the host-side mirror keeps for
	// this project. Nil inherits the [repos] default. It is a separate knob
	// from CloneDepth: that one bounds what each job and its VM see, this one
	// bounds what the host caches on their behalf. A mirror shallower than
	// CloneDepth cannot serve it, so the mirror is deepened to match.
	MirrorDepth *int
	// The following cap what this project may hold *at once*, summed across
	// its running jobs, so one busy project cannot take the whole host. Each
	// is nil/empty to inherit the [scheduler.per_project] default, and an
	// explicit zero to mean unlimited even when a default is set. A job over
	// the cap waits without blocking other projects.
	MaxJobs   *int
	MaxCPUs   *uint
	MaxMemory string
	MaxDisk   string
	// Priority ranks this project's queued jobs against other projects',
	// higher first. Nil is the default of zero. It orders the queue and never
	// reserves capacity, and a waiting job gains priority over time, so a
	// high-priority project cannot starve a low-priority one. Per-job-mode
	// overrides live in Jobs.
	Priority *int
}

// Preview is the per-project preview policy. It sits between the operator's
// [preview] section and the repository's kvarn.yml: the operator decides
// whether the feature exists at all, this decides whether this project may use
// it, and the repository decides what it looks like.
type Preview struct {
	// Enabled turns previews on for this project. Nil inherits the operator's
	// stance, which is on whenever the [preview] section is configured.
	Enabled *bool
	// Domain overrides the operator's base domain for this project, for giving
	// one repository its own zone. Empty inherits.
	Domain string
	// AllowForks permits previews for refs that came from a fork. It is off by
	// default: a preview runs the branch's own code with the project's
	// resolved secrets, and a fork's branch is written by someone who does not
	// have push access to this repository.
	AllowForks *bool
	// AutoStart are the hostname patterns that start a preview by being asked
	// for. Each must use `{pr}` exactly once and end in `.{domain}`, so a
	// request for `pr-12.preview.example.com` names the pull request to preview
	// without anything having registered it first.
	//
	// It is configured here rather than in kvarn.yml because the mapping has to
	// be known before the repository is cloned — there is no checkout to read
	// when the first request for an unclaimed hostname arrives. The repository
	// still decides what the preview looks like, and its sites must resolve to
	// the same names for the hostname to route once the boot finishes.
	//
	// Empty means previews for this project start only when something asks for
	// them by name, through `kvarn preview up` or the API.
	AutoStart []string
}

// Store provides CRUD operations for projects. Get and Delete return
// tomlstore.ErrNotFound when no entry matches.
type Store interface {
	Get(ctx context.Context, name string) (*Project, error)
	List(ctx context.Context) ([]*Project, error)
	Put(ctx context.Context, p *Project) error
	Delete(ctx context.Context, name string) error
}
