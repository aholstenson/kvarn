package tomlstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/aholstenson/kvarn/internal/config/atomicfile"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/pelletier/go-toml/v2"
)

type fileData struct {
	Projects map[string]*projectEntry `toml:"projects"`
}

type jobEntry struct {
	MaxCostUSD *float64 `toml:"max_cost_usd,omitempty"`
}

type projectEntry struct {
	Repo              string              `toml:"repo"`
	DefaultBranch     string              `toml:"default_branch,omitempty"`
	Forge             string              `toml:"forge,omitempty"`
	MaxCostUSD        *float64            `toml:"max_cost_usd,omitempty"`
	ReportCostOnPR    *bool               `toml:"report_cost_on_pr,omitempty"`
	Jobs              map[string]jobEntry `toml:"jobs,omitempty"`
	BranchPrefix      string              `toml:"branch_prefix,omitempty"`
	Labels            []string            `toml:"labels,omitempty"`
	CommitAuthorName  string              `toml:"commit_author_name,omitempty"`
	CommitAuthorEmail string              `toml:"commit_author_email,omitempty"`
}

// Store is a TOML file-backed project store.
type Store struct {
	path string
	mu   sync.RWMutex
}

// New creates a Store backed by the given file path.
func New(path string) *Store {
	return &Store{path: path}
}

// DefaultPath returns the default project store path.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kvarn", "projects.toml")
}

func (s *Store) load() (*fileData, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &fileData{Projects: make(map[string]*projectEntry)}, nil
		}
		return nil, err
	}

	var fd fileData
	if err := toml.Unmarshal(data, &fd); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	if fd.Projects == nil {
		fd.Projects = make(map[string]*projectEntry)
	}
	return &fd, nil
}

func (s *Store) save(fd *fileData) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}

	data, err := toml.Marshal(fd)
	if err != nil {
		return err
	}

	return atomicfile.Write(s.path, data, 0644)
}

func entryToProject(name string, entry *projectEntry) *project.Project {
	var jobs map[string]project.JobLimits
	if len(entry.Jobs) > 0 {
		jobs = make(map[string]project.JobLimits, len(entry.Jobs))
		for mode, j := range entry.Jobs {
			jobs[mode] = project.JobLimits{MaxCostUSD: j.MaxCostUSD}
		}
	}
	labels := make([]string, len(entry.Labels))
	copy(labels, entry.Labels)
	return &project.Project{
		Name:              name,
		RepoURL:           entry.Repo,
		DefaultBranch:     entry.DefaultBranch,
		Forge:             entry.Forge,
		MaxCostUSD:        entry.MaxCostUSD,
		ReportCostOnPR:    entry.ReportCostOnPR,
		Jobs:              jobs,
		BranchPrefix:      entry.BranchPrefix,
		Labels:            labels,
		CommitAuthorName:  entry.CommitAuthorName,
		CommitAuthorEmail: entry.CommitAuthorEmail,
	}
}

func projectToEntry(p *project.Project) *projectEntry {
	var jobs map[string]jobEntry
	if len(p.Jobs) > 0 {
		jobs = make(map[string]jobEntry, len(p.Jobs))
		for mode, j := range p.Jobs {
			jobs[mode] = jobEntry{MaxCostUSD: j.MaxCostUSD}
		}
	}
	return &projectEntry{
		Repo:              p.RepoURL,
		DefaultBranch:     p.DefaultBranch,
		Forge:             p.Forge,
		MaxCostUSD:        p.MaxCostUSD,
		ReportCostOnPR:    p.ReportCostOnPR,
		Jobs:              jobs,
		BranchPrefix:      p.BranchPrefix,
		Labels:            p.Labels,
		CommitAuthorName:  p.CommitAuthorName,
		CommitAuthorEmail: p.CommitAuthorEmail,
	}
}

func (s *Store) Get(_ context.Context, name string) (*project.Project, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fd, err := s.load()
	if err != nil {
		return nil, err
	}

	entry, ok := fd.Projects[name]
	if !ok {
		return nil, fmt.Errorf("project %q not found", name)
	}

	return entryToProject(name, entry), nil
}

func (s *Store) List(_ context.Context) ([]*project.Project, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fd, err := s.load()
	if err != nil {
		return nil, err
	}

	var result []*project.Project
	for name, entry := range fd.Projects {
		result = append(result, entryToProject(name, entry))
	}
	return result, nil
}

func (s *Store) Put(_ context.Context, p *project.Project) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return atomicfile.WithLock(s.path, func() error {
		fd, err := s.load()
		if err != nil {
			return err
		}

		fd.Projects[p.Name] = projectToEntry(p)

		return s.save(fd)
	})
}

func (s *Store) Delete(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return atomicfile.WithLock(s.path, func() error {
		fd, err := s.load()
		if err != nil {
			return err
		}

		if _, ok := fd.Projects[name]; !ok {
			return fmt.Errorf("project %q not found", name)
		}

		delete(fd.Projects, name)
		return s.save(fd)
	})
}
