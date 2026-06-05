package tomlstore

import (
	"context"
	"os"
	"path/filepath"

	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/config/tomlstore"
)

type fileData struct {
	Projects map[string]*projectEntry `toml:"projects"`
}

type jobEntry struct {
	MaxCostUSD           *float64 `toml:"max_cost_usd,omitempty"`
	MaxValidationRetries *int     `toml:"max_validation_retries,omitempty"`
}

type projectEntry struct {
	Repo                 string              `toml:"repo"`
	DefaultBranch        string              `toml:"default_branch,omitempty"`
	Forge                string              `toml:"forge,omitempty"`
	MaxCostUSD           *float64            `toml:"max_cost_usd,omitempty"`
	ReportCostOnPR       *bool               `toml:"report_cost_on_pr,omitempty"`
	MaxValidationRetries *int                `toml:"max_validation_retries,omitempty"`
	Jobs                 map[string]jobEntry `toml:"jobs,omitempty"`
	BranchPrefix         string              `toml:"branch_prefix,omitempty"`
	Labels               []string            `toml:"labels,omitempty"`
	CommitAuthorName     string              `toml:"commit_author_name,omitempty"`
	CommitAuthorEmail    string              `toml:"commit_author_email,omitempty"`
	CloneDepth           *int                `toml:"clone_depth,omitempty"`
}

// Store is a TOML file-backed project store.
type Store struct {
	inner *tomlstore.Store[string, fileData, *projectEntry, *project.Project]
}

// New creates a Store backed by the given file path.
func New(path string) *Store {
	return &Store{inner: tomlstore.New(
		path,
		tomlstore.Config,
		tomlstore.Schema[string, fileData, *projectEntry]{
			NewFileData: func() fileData {
				return fileData{Projects: map[string]*projectEntry{}}
			},
			Get: func(fd fileData, k string) (*projectEntry, bool) {
				e, ok := fd.Projects[k]
				return e, ok
			},
			Put: func(fd *fileData, k string, e *projectEntry) {
				if fd.Projects == nil {
					fd.Projects = map[string]*projectEntry{}
				}
				fd.Projects[k] = e
			},
			Delete: func(fd *fileData, k string) bool {
				if _, ok := fd.Projects[k]; !ok {
					return false
				}
				delete(fd.Projects, k)
				return true
			},
			Keys: func(fd fileData) []string {
				ks := make([]string, 0, len(fd.Projects))
				for k := range fd.Projects {
					ks = append(ks, k)
				}
				return ks
			},
			Less: func(a, b string) bool { return a < b },
		},
		entryToProject,
		projectToEntry,
	)}
}

// DefaultPath returns the default project store path.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kvarn", "projects.toml")
}

func entryToProject(name string, entry *projectEntry) (*project.Project, error) {
	var jobs map[string]project.JobLimits
	if len(entry.Jobs) > 0 {
		jobs = make(map[string]project.JobLimits, len(entry.Jobs))
		for mode, j := range entry.Jobs {
			jobs[mode] = project.JobLimits{
				MaxCostUSD:           j.MaxCostUSD,
				MaxValidationRetries: j.MaxValidationRetries,
			}
		}
	}
	labels := make([]string, len(entry.Labels))
	copy(labels, entry.Labels)
	return &project.Project{
		Name:                 name,
		RepoURL:              entry.Repo,
		DefaultBranch:        entry.DefaultBranch,
		Forge:                entry.Forge,
		MaxCostUSD:           entry.MaxCostUSD,
		ReportCostOnPR:       entry.ReportCostOnPR,
		MaxValidationRetries: entry.MaxValidationRetries,
		Jobs:                 jobs,
		BranchPrefix:         entry.BranchPrefix,
		Labels:               labels,
		CommitAuthorName:     entry.CommitAuthorName,
		CommitAuthorEmail:    entry.CommitAuthorEmail,
		CloneDepth:           entry.CloneDepth,
	}, nil
}

func projectToEntry(p *project.Project) (string, *projectEntry) {
	var jobs map[string]jobEntry
	if len(p.Jobs) > 0 {
		jobs = make(map[string]jobEntry, len(p.Jobs))
		for mode, j := range p.Jobs {
			jobs[mode] = jobEntry{
				MaxCostUSD:           j.MaxCostUSD,
				MaxValidationRetries: j.MaxValidationRetries,
			}
		}
	}
	return p.Name, &projectEntry{
		Repo:                 p.RepoURL,
		DefaultBranch:        p.DefaultBranch,
		Forge:                p.Forge,
		MaxCostUSD:           p.MaxCostUSD,
		ReportCostOnPR:       p.ReportCostOnPR,
		MaxValidationRetries: p.MaxValidationRetries,
		Jobs:                 jobs,
		BranchPrefix:         p.BranchPrefix,
		Labels:               p.Labels,
		CommitAuthorName:     p.CommitAuthorName,
		CommitAuthorEmail:    p.CommitAuthorEmail,
		CloneDepth:           p.CloneDepth,
	}
}

func (s *Store) Get(ctx context.Context, name string) (*project.Project, error) {
	return s.inner.Get(ctx, name)
}

func (s *Store) List(ctx context.Context) ([]*project.Project, error) {
	return s.inner.List(ctx)
}

func (s *Store) Put(ctx context.Context, p *project.Project) error {
	return s.inner.Put(ctx, p)
}

func (s *Store) Delete(ctx context.Context, name string) error {
	return s.inner.Delete(ctx, name)
}
