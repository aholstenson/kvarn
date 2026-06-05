package tomlstore

import (
	"context"
	"os"
	"path/filepath"

	llms "github.com/aholstenson/llms-go"

	modelcfg "github.com/aholstenson/kvarn/internal/config/model"
	"github.com/aholstenson/kvarn/internal/config/tomlstore"
)

// entryData mirrors a single [models.<alias>] block in agents.toml.
type entryData struct {
	Model           string       `toml:"model"`
	ReasoningEffort *llms.Effort `toml:"reasoning_effort"`
	MaxOutputTokens *int         `toml:"max_output_tokens"`
}

// jobDefaults mirrors a single [defaults.jobs.<mode>] block.
type jobDefaults struct {
	MaxCostUSD           *float64 `toml:"max_cost_usd,omitempty"`
	MaxValidationRetries *int     `toml:"max_validation_retries,omitempty"`
}

// defaultsData mirrors the [defaults] block. The Jobs map carries per-mode
// overrides keyed by mode name (auto, implement, fix, review, research).
type defaultsData struct {
	MaxCostUSD           *float64               `toml:"max_cost_usd,omitempty"`
	WarnThreshold        *float64               `toml:"warn_threshold,omitempty"`
	ReportCostOnPR       *bool                  `toml:"report_cost_on_pr,omitempty"`
	MaxValidationRetries *int                   `toml:"max_validation_retries,omitempty"`
	Jobs                 map[string]jobDefaults `toml:"jobs,omitempty"`
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

// modelDomain is the per-alias domain value List would return. Model only uses
// the full-map All() accessor (List is unused), but the generic store still
// requires entry⇄domain callbacks; this struct carries the alias alongside
// the raw entry so List would behave correctly if a caller ever reached for it.
type modelDomain struct {
	Alias string
	Raw   modelcfg.RawEntry
}

// Store is a TOML file-backed model-alias override store. It is effectively
// read-only — agents.toml is hand-edited — so Put/Delete are unused, though
// the generic store exposes them.
type Store struct {
	inner *tomlstore.Store[string, fileData, entryData, modelDomain]
}

// New creates a Store backed by the given file path.
func New(path string) *Store {
	return &Store{inner: tomlstore.New(
		path,
		tomlstore.Config,
		tomlstore.Schema[string, fileData, entryData]{
			NewFileData: func() fileData {
				return fileData{Models: map[string]entryData{}}
			},
			Get: func(fd fileData, k string) (entryData, bool) {
				e, ok := fd.Models[k]
				return e, ok
			},
			Put: func(fd *fileData, k string, e entryData) {
				if fd.Models == nil {
					fd.Models = map[string]entryData{}
				}
				fd.Models[k] = e
			},
			Delete: func(fd *fileData, k string) bool {
				if _, ok := fd.Models[k]; !ok {
					return false
				}
				delete(fd.Models, k)
				return true
			},
			Keys: func(fd fileData) []string {
				ks := make([]string, 0, len(fd.Models))
				for k := range fd.Models {
					ks = append(ks, k)
				}
				return ks
			},
			Less: func(a, b string) bool { return a < b },
		},
		func(alias string, e entryData) (modelDomain, error) {
			return modelDomain{
				Alias: alias,
				Raw: modelcfg.RawEntry{
					ModelID:         e.Model,
					ReasoningEffort: e.ReasoningEffort,
					MaxOutputTokens: e.MaxOutputTokens,
				},
			}, nil
		},
		func(d modelDomain) (string, entryData) {
			return d.Alias, entryData{
				Model:           d.Raw.ModelID,
				ReasoningEffort: d.Raw.ReasoningEffort,
				MaxOutputTokens: d.Raw.MaxOutputTokens,
			}
		},
	)}
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

// All returns every model alias override. A missing file is not an error;
// callers receive an empty map.
func (s *Store) All(ctx context.Context) (map[string]modelcfg.RawEntry, error) {
	fd, err := s.inner.Load(ctx)
	if err != nil {
		return nil, err
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
// file yields a zero-value Defaults; a malformed file surfaces as an error.
func (s *Store) Defaults(ctx context.Context) (modelcfg.Defaults, error) {
	fd, err := s.inner.Load(ctx)
	if err != nil {
		return modelcfg.Defaults{}, err
	}
	out := modelcfg.Defaults{
		MaxCostUSD:           fd.Defaults.MaxCostUSD,
		WarnThreshold:        fd.Defaults.WarnThreshold,
		ReportCostOnPR:       fd.Defaults.ReportCostOnPR,
		MaxValidationRetries: fd.Defaults.MaxValidationRetries,
	}
	if len(fd.Defaults.Jobs) > 0 {
		out.Jobs = make(map[string]modelcfg.JobDefaults, len(fd.Defaults.Jobs))
		for mode, j := range fd.Defaults.Jobs {
			out.Jobs[mode] = modelcfg.JobDefaults{
				MaxCostUSD:           j.MaxCostUSD,
				MaxValidationRetries: j.MaxValidationRetries,
			}
		}
	}
	return out, nil
}
