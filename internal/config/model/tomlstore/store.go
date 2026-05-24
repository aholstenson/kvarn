package tomlstore

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"fmt"

	"github.com/pelletier/go-toml/v2"

	llms "github.com/aholstenson/llms-go"

	modelcfg "github.com/aholstenson/kvarn/internal/config/model"
)

// entryData mirrors a single [models.<alias>] block in agents.toml.
type entryData struct {
	Model           string       `toml:"model"`
	ReasoningEffort *llms.Effort `toml:"reasoning_effort"`
	MaxOutputTokens *int         `toml:"max_output_tokens"`
}

// jobDefaults mirrors a single [defaults.jobs.<mode>] block.
type jobDefaults struct {
	MaxCostUSD *float64 `toml:"max_cost_usd,omitempty"`
}

// defaultsData mirrors the [defaults] block. The Jobs map carries per-mode
// overrides keyed by mode name (auto, implement, fix, review, research).
type defaultsData struct {
	MaxCostUSD     *float64               `toml:"max_cost_usd,omitempty"`
	WarnThreshold  *float64               `toml:"warn_threshold,omitempty"`
	ReportCostOnPR *bool                  `toml:"report_cost_on_pr,omitempty"`
	Jobs           map[string]jobDefaults `toml:"jobs,omitempty"`
}

// fileData mirrors the on-disk layout:
//
//	[defaults]
//	max_cost_usd      = 5.00
//	warn_threshold    = 0.80
//	report_cost_on_pr = true
//
//	[defaults.jobs.implement]
//	max_cost_usd = 25.00
//
//	[models.coding-agent]
//	model            = "anthropic/claude-sonnet-4-6"
//	reasoning_effort = "medium"
//	max_output_tokens = 16384
type fileData struct {
	Defaults defaultsData         `toml:"defaults"`
	Models   map[string]entryData `toml:"models"`
}

// Store is a TOML file-backed model-alias override store.
type Store struct {
	path string
	mu   sync.RWMutex
}

// New creates a Store backed by the given file path.
func New(path string) *Store {
	return &Store{path: path}
}

// DefaultPath returns the default agents config path.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kvarn", "agents.toml")
}

// OpenDefault returns a Store backed by path, or by DefaultPath() when path
// is empty.
func OpenDefault(path string) *Store {
	if path == "" {
		path = DefaultPath()
	}
	return New(path)
}

func (s *Store) load() (fileData, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return fileData{}, nil
		}
		return fileData{}, err
	}
	var fd fileData
	if err := toml.Unmarshal(data, &fd); err != nil {
		return fileData{}, fmt.Errorf("parse %s: %w", s.path, err)
	}
	return fd, nil
}

func (s *Store) All(_ context.Context) (map[string]modelcfg.RawEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fd, err := s.load()
	if err != nil {
		return nil, err
	}
	if fd.Models == nil {
		return map[string]modelcfg.RawEntry{}, nil
	}

	out := make(map[string]modelcfg.RawEntry, len(fd.Models))
	for alias, e := range fd.Models {
		out[alias] = modelcfg.RawEntry{
			ModelID:         e.Model,
			ReasoningEffort: e.ReasoningEffort,
			MaxOutputTokens: e.MaxOutputTokens,
		}
	}
	return out, nil
}

// Defaults returns the parsed [defaults] block from agents.toml. A missing
// file yields a zero-value Defaults with no error so callers can layer
// built-in fallbacks on top.
func (s *Store) Defaults(_ context.Context) (modelcfg.Defaults, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fd, err := s.load()
	if err != nil {
		return modelcfg.Defaults{}, err
	}

	out := modelcfg.Defaults{
		MaxCostUSD:     fd.Defaults.MaxCostUSD,
		WarnThreshold:  fd.Defaults.WarnThreshold,
		ReportCostOnPR: fd.Defaults.ReportCostOnPR,
	}
	if len(fd.Defaults.Jobs) > 0 {
		out.Jobs = make(map[string]modelcfg.JobDefaults, len(fd.Defaults.Jobs))
		for mode, j := range fd.Defaults.Jobs {
			out.Jobs[mode] = modelcfg.JobDefaults{MaxCostUSD: j.MaxCostUSD}
		}
	}
	return out, nil
}
