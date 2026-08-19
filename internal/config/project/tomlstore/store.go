package tomlstore

import (
	"context"
	"os"
	"path/filepath"

	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/config/tomlstore"
)

type fileData struct {
	Projects map[string]*projectEntry `toml:"projects"`
}

type jobEntry struct {
	MaxCostUSD           *float64 `toml:"max_cost_usd,omitempty"`
	MaxValidationRetries *int     `toml:"max_validation_retries,omitempty"`
	Priority             *int     `toml:"priority,omitempty"`
}

type projectEntry struct {
	Repo                 string              `toml:"repo"`
	DefaultBranch        string              `toml:"default_branch,omitempty"`
	Forge                string              `toml:"forge,omitempty"`
	MaxCostUSD           *float64            `toml:"max_cost_usd,omitempty"`
	ReportCostOnPR       *bool               `toml:"report_cost_on_pr,omitempty"`
	ReportWorklogOnPR    *bool               `toml:"report_worklog_on_pr,omitempty"`
	MaxValidationRetries *int                `toml:"max_validation_retries,omitempty"`
	Jobs                 map[string]jobEntry `toml:"jobs,omitempty"`
	BranchPrefix         string              `toml:"branch_prefix,omitempty"`
	Labels               []string            `toml:"labels,omitempty"`
	CommitAuthorName     string              `toml:"commit_author_name,omitempty"`
	CommitAuthorEmail    string              `toml:"commit_author_email,omitempty"`
	CloneDepth           *int                `toml:"clone_depth,omitempty"`
	MirrorDepth          *int                `toml:"mirror_depth,omitempty"`
	MaxJobs              *int                `toml:"max_jobs,omitempty"`
	MaxCPUs              *uint               `toml:"max_cpu,omitempty"`
	MaxMemory            string              `toml:"max_memory,omitempty"`
	MaxDisk              string              `toml:"max_disk,omitempty"`
	Priority             *int                `toml:"priority,omitempty"`
	PullRequest          *prEntry            `toml:"pull_request,omitempty"`
	Preview              *previewEntry       `toml:"preview,omitempty"`
}

// previewEntry mirrors the `[projects.<name>.preview]` block. Like prEntry it
// is a pointer so a config without the block round-trips through Put without
// gaining an empty table.
type previewEntry struct {
	Enabled    *bool    `toml:"enabled,omitempty"`
	Domain     string   `toml:"domain,omitempty"`
	AllowForks *bool    `toml:"allow_forks,omitempty"`
	AutoStart  []string `toml:"auto_start,omitempty"`
}

// toPreview converts a parsed block to the domain type. A nil receiver is the
// absent block and yields a zero Preview, which inherits everything.
func (e *previewEntry) toPreview() project.Preview {
	if e == nil {
		return project.Preview{}
	}
	return project.Preview{
		Enabled:    e.Enabled,
		Domain:     e.Domain,
		AllowForks: e.AllowForks,
		AutoStart:  e.AutoStart,
	}
}

// previewEntryFrom converts the domain type back to a parsed block, returning
// nil when nothing is set.
func previewEntryFrom(p project.Preview) *previewEntry {
	if p.Enabled == nil && p.Domain == "" && p.AllowForks == nil && len(p.AutoStart) == 0 {
		return nil
	}
	return &previewEntry{
		Enabled:    p.Enabled,
		Domain:     p.Domain,
		AllowForks: p.AllowForks,
		AutoStart:  p.AutoStart,
	}
}

// prEntry mirrors the `[projects.<name>.pull_request]` block. It is a pointer
// so a config without the block round-trips through Put without gaining an
// empty table.
//
// The same block exists in forges.toml at two levels; the shape is restated
// here rather than shared because each store owns its own file format, the way
// every other entry type in these packages does.
type prEntry struct {
	TitleInstructions   string                `toml:"title_instructions,omitempty"`
	TitleMaxLength      *int                  `toml:"title_max_length,omitempty"`
	BodyInstructions    string                `toml:"body_instructions,omitempty"`
	BodyFooter          string                `toml:"body_footer,omitempty"`
	CommentInstructions string                `toml:"comment_instructions,omitempty"`
	CommentHeaders      *headerEntry          `toml:"comment_headers,omitempty"`
	CommitTrailers      []string              `toml:"commit_trailers,omitempty"`
	ReportWorklogOnPR   *bool                 `toml:"report_worklog_on_pr,omitempty"`
	ReportCostOnPR      *bool                 `toml:"report_cost_on_pr,omitempty"`
	QuoteTask           forgeconfig.QuoteMode `toml:"quote_task,omitempty"`
}

// headerEntry mirrors a `comment_headers` sub-table, one key per kind of
// comment a delivery posts.
type headerEntry struct {
	NewPullRequest string `toml:"new_pull_request,omitempty"`
	FollowUpCommit string `toml:"follow_up_commit,omitempty"`
	PRComment      string `toml:"pr_comment,omitempty"`
}

func (e *headerEntry) toHeaders() forgeconfig.CommentHeaders {
	if e == nil {
		return forgeconfig.CommentHeaders{}
	}
	return forgeconfig.CommentHeaders{
		NewPullRequest: e.NewPullRequest,
		FollowUpCommit: e.FollowUpCommit,
		PRComment:      e.PRComment,
	}
}

// headerEntryFrom returns nil for headers that set nothing, so an untouched
// config does not grow the sub-table.
func headerEntryFrom(h forgeconfig.CommentHeaders) *headerEntry {
	if h.Empty() {
		return nil
	}
	return &headerEntry{
		NewPullRequest: h.NewPullRequest,
		FollowUpCommit: h.FollowUpCommit,
		PRComment:      h.PRComment,
	}
}

// toContent converts a parsed block to the domain type. A nil receiver is the
// absent block and yields a zero PRContent, which contributes nothing.
func (e *prEntry) toContent() forgeconfig.PRContent {
	if e == nil {
		return forgeconfig.PRContent{}
	}
	trailers := make([]string, len(e.CommitTrailers))
	copy(trailers, e.CommitTrailers)
	return forgeconfig.PRContent{
		TitleInstructions:   e.TitleInstructions,
		TitleMaxLength:      e.TitleMaxLength,
		BodyInstructions:    e.BodyInstructions,
		BodyFooter:          e.BodyFooter,
		CommentInstructions: e.CommentInstructions,
		CommentHeaders:      e.CommentHeaders.toHeaders(),
		CommitTrailers:      trailers,
		ReportWorklog:       e.ReportWorklogOnPR,
		ReportCost:          e.ReportCostOnPR,
		QuoteTask:           e.QuoteTask,
	}
}

// prEntryFrom converts the domain type back to a parsed block, returning nil
// when nothing is set so an untouched config does not grow the table.
func prEntryFrom(c forgeconfig.PRContent) *prEntry {
	e := &prEntry{
		TitleInstructions:   c.TitleInstructions,
		TitleMaxLength:      c.TitleMaxLength,
		BodyInstructions:    c.BodyInstructions,
		BodyFooter:          c.BodyFooter,
		CommentInstructions: c.CommentInstructions,
		CommentHeaders:      headerEntryFrom(c.CommentHeaders),
		ReportWorklogOnPR:   c.ReportWorklog,
		ReportCostOnPR:      c.ReportCost,
		QuoteTask:           c.QuoteTask,
	}
	if len(c.CommitTrailers) > 0 {
		e.CommitTrailers = make([]string, len(c.CommitTrailers))
		copy(e.CommitTrailers, c.CommitTrailers)
	}
	if e.isZero() {
		return nil
	}
	return e
}

// isZero reports whether the block sets nothing. It is written out by hand
// because a slice field makes prEntry uncomparable.
func (e *prEntry) isZero() bool {
	return e.TitleInstructions == "" &&
		e.TitleMaxLength == nil &&
		e.BodyInstructions == "" &&
		e.BodyFooter == "" &&
		e.CommentInstructions == "" &&
		e.CommentHeaders == nil &&
		len(e.CommitTrailers) == 0 &&
		e.ReportWorklogOnPR == nil &&
		e.ReportCostOnPR == nil &&
		e.QuoteTask == forgeconfig.QuoteInherit
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
				Priority:             j.Priority,
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
		ReportWorklogOnPR:    entry.ReportWorklogOnPR,
		MaxValidationRetries: entry.MaxValidationRetries,
		Jobs:                 jobs,
		BranchPrefix:         entry.BranchPrefix,
		Labels:               labels,
		CommitAuthorName:     entry.CommitAuthorName,
		CommitAuthorEmail:    entry.CommitAuthorEmail,
		CloneDepth:           entry.CloneDepth,
		MirrorDepth:          entry.MirrorDepth,
		MaxJobs:              entry.MaxJobs,
		MaxCPUs:              entry.MaxCPUs,
		MaxMemory:            entry.MaxMemory,
		MaxDisk:              entry.MaxDisk,
		Priority:             entry.Priority,
		PullRequest:          entry.PullRequest.toContent(),
		Preview:              entry.Preview.toPreview(),
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
				Priority:             j.Priority,
			}
		}
	}
	return p.Name, &projectEntry{
		Repo:                 p.RepoURL,
		DefaultBranch:        p.DefaultBranch,
		Forge:                p.Forge,
		MaxCostUSD:           p.MaxCostUSD,
		ReportCostOnPR:       p.ReportCostOnPR,
		ReportWorklogOnPR:    p.ReportWorklogOnPR,
		MaxValidationRetries: p.MaxValidationRetries,
		Jobs:                 jobs,
		BranchPrefix:         p.BranchPrefix,
		Labels:               p.Labels,
		CommitAuthorName:     p.CommitAuthorName,
		CommitAuthorEmail:    p.CommitAuthorEmail,
		CloneDepth:           p.CloneDepth,
		MirrorDepth:          p.MirrorDepth,
		MaxJobs:              p.MaxJobs,
		MaxCPUs:              p.MaxCPUs,
		MaxMemory:            p.MaxMemory,
		MaxDisk:              p.MaxDisk,
		Priority:             p.Priority,
		PullRequest:          prEntryFrom(p.PullRequest),
		Preview:              previewEntryFrom(p.Preview),
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
