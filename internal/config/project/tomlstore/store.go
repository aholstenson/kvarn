package tomlstore

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/cockroachdb/errors"
	"github.com/pelletier/go-toml/v2"
)

type fileData struct {
	Projects map[string]*projectEntry `toml:"projects"`
}

type jobEntry struct {
	MaxCostUSD *float64 `toml:"max_cost_usd,omitempty"`
}

type projectEntry struct {
	Repo            string              `toml:"repo"`
	DefaultBranch   string              `toml:"default_branch,omitempty"`
	Forge           string              `toml:"forge,omitempty"`
	MaxCostUSD      *float64            `toml:"max_cost_usd,omitempty"`
	ReportCostOnPR  *bool               `toml:"report_cost_on_pr,omitempty"`
	Jobs            map[string]jobEntry `toml:"jobs,omitempty"`
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
		return nil, errors.Wrapf(err, "parse %s", s.path)
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

	return os.WriteFile(s.path, data, 0644)
}

func entryToProject(name string, entry *projectEntry) *project.Project {
	var jobs map[string]project.JobLimits
	if len(entry.Jobs) > 0 {
		jobs = make(map[string]project.JobLimits, len(entry.Jobs))
		for mode, j := range entry.Jobs {
			jobs[mode] = project.JobLimits{MaxCostUSD: j.MaxCostUSD}
		}
	}
	return &project.Project{
		Name:           name,
		RepoURL:        entry.Repo,
		DefaultBranch:  entry.DefaultBranch,
		Forge:          entry.Forge,
		MaxCostUSD:     entry.MaxCostUSD,
		ReportCostOnPR: entry.ReportCostOnPR,
		Jobs:           jobs,
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
		Repo:           p.RepoURL,
		DefaultBranch:  p.DefaultBranch,
		Forge:          p.Forge,
		MaxCostUSD:     p.MaxCostUSD,
		ReportCostOnPR: p.ReportCostOnPR,
		Jobs:           jobs,
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
		return nil, errors.Newf("project %q not found", name)
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

	fd, err := s.load()
	if err != nil {
		return err
	}

	fd.Projects[p.Name] = projectToEntry(p)

	return s.save(fd)
}

func (s *Store) Delete(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	fd, err := s.load()
	if err != nil {
		return err
	}

	if _, ok := fd.Projects[name]; !ok {
		return errors.Newf("project %q not found", name)
	}

	delete(fd.Projects, name)
	return s.save(fd)
}
