package model

import (
	"context"

	"fmt"

	llms "github.com/aholstenson/llms-go"
)

// Entry holds the resolved configuration for a model alias.
type Entry struct {
	ModelID         string
	ReasoningEffort llms.Effort // "" = none
	MaxOutputTokens int         // 0 = use caller default
	MaxSteps        int         // 0 = use caller default
}

// RawEntry is the user-supplied override for a single model alias. Pointer
// fields are nil when the key is absent from the config file, preserving
// the compiled-in default.
type RawEntry struct {
	ModelID         string
	ReasoningEffort *llms.Effort
	MaxOutputTokens *int
	MaxSteps        *int
}

// Store reads model-alias configuration from user config. An empty result
// (e.g. when the backing file does not exist) is not an error — callers are
// expected to layer overrides on top of built-in defaults.
type Store interface {
	All(ctx context.Context) (map[string]RawEntry, error)
}

// JobDefaults is the per-job-mode default block (forward-compatible: today
// only cost cap, later per-mode model selection).
type JobDefaults struct {
	MaxCostUSD           *float64
	MaxValidationRetries *int
}

// Defaults holds the top-level user defaults that apply to all jobs unless a
// project or per-mode override is set. All fields are optional; nil means
// "use the built-in fallback".
type Defaults struct {
	MaxCostUSD           *float64
	WarnThreshold        *float64
	ReportCostOnPR       *bool
	MaxValidationRetries *int
	Jobs                 map[string]JobDefaults
}

// DefaultsStore reads user-level defaults (the [defaults] block in
// agents.toml). An empty/missing config is not an error: callers receive a
// zero-value Defaults struct and apply built-in fallbacks themselves.
type DefaultsStore interface {
	Defaults(ctx context.Context) (Defaults, error)
}

// Resolve registers each alias in defaults on mgr (overrides layered on top),
// then resolves every alias to a concrete llms.Model. It returns both the
// model map and the merged configuration keyed by alias.
// mainAlias names the "primary" alias; when mainOverride is non-empty it picks
// which model that alias resolves to, allowing CLI --model flag overrides.
func Resolve(
	ctx context.Context,
	mgr *llms.Manager,
	store Store,
	defaults map[string]Entry,
	mainAlias, mainOverride string,
) (map[string]llms.Model, map[string]Entry, error) {
	overrides, err := store.All(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("load model config: %w", err)
	}

	resolved := make(map[string]Entry, len(defaults))
	for alias, def := range defaults {
		entry := def
		if raw, ok := overrides[alias]; ok {
			if raw.ModelID != "" {
				entry.ModelID = raw.ModelID
			}
			if raw.ReasoningEffort != nil {
				entry.ReasoningEffort = *raw.ReasoningEffort
			}
			if raw.MaxOutputTokens != nil {
				entry.MaxOutputTokens = *raw.MaxOutputTokens
			}
			if raw.MaxSteps != nil {
				entry.MaxSteps = *raw.MaxSteps
			}
		}
		mgr.RegisterAlias(alias, entry.ModelID)
		resolved[alias] = entry
	}

	models := make(map[string]llms.Model, len(defaults))
	for alias := range defaults {
		name := alias
		if alias == mainAlias && mainOverride != "" {
			name = mainOverride
		}
		m, err := mgr.GetModel(ctx, name)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve model %q: %w", name, err)
		}
		models[alias] = m
	}
	return models, resolved, nil
}
