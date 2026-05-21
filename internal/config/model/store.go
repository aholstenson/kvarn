package model

import (
	"context"

	llms "github.com/aholstenson/llms-go"
	"github.com/cockroachdb/errors"
)

// Entry holds the resolved configuration for a model alias.
type Entry struct {
	ModelID         string
	ThinkingTokens  int // 0 = disabled
	MaxOutputTokens int // 0 = use caller default
}

// RawEntry is the user-supplied override for a single model alias. Pointer
// fields are nil when the key is absent from the config file, preserving
// the compiled-in default.
type RawEntry struct {
	ModelID         string
	ThinkingTokens  *int
	MaxOutputTokens *int
}

// Store reads model-alias configuration from user config. An empty result
// (e.g. when the backing file does not exist) is not an error — callers are
// expected to layer overrides on top of built-in defaults.
type Store interface {
	All(ctx context.Context) (map[string]RawEntry, error)
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
		return nil, nil, errors.Wrap(err, "load model config")
	}

	resolved := make(map[string]Entry, len(defaults))
	for alias, def := range defaults {
		entry := def
		if raw, ok := overrides[alias]; ok {
			if raw.ModelID != "" {
				entry.ModelID = raw.ModelID
			}
			if raw.ThinkingTokens != nil {
				entry.ThinkingTokens = *raw.ThinkingTokens
			}
			if raw.MaxOutputTokens != nil {
				entry.MaxOutputTokens = *raw.MaxOutputTokens
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
			return nil, nil, errors.Wrapf(err, "resolve model %q", name)
		}
		models[alias] = m
	}
	return models, resolved, nil
}
