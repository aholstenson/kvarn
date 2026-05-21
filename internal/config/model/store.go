package model

import (
	"context"

	llms "github.com/aholstenson/llms-go"
	"github.com/cockroachdb/errors"
)

// Store reads model-alias overrides from user configuration. An empty result
// (e.g. when the backing file does not exist) is not an error — callers are
// expected to layer overrides on top of built-in defaults.
type Store interface {
	// All returns the configured alias → provider/model-id overrides.
	All(ctx context.Context) (map[string]string, error)
}

// Resolve registers each alias in defaults on mgr (overrides layered on top),
// then resolves every alias to a concrete llms.Model. mainAlias names the
// "primary" alias; when mainOverride is non-empty it picks which model that
// alias resolves to (an alias name or raw provider/model id), which is how
// the CLI --model flag overrides the main model per invocation.
func Resolve(
	ctx context.Context,
	mgr *llms.Manager,
	store Store,
	defaults map[string]string,
	mainAlias, mainOverride string,
) (map[string]llms.Model, error) {
	overrides, err := store.All(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "load model config")
	}

	for alias, modelID := range defaults {
		if v, ok := overrides[alias]; ok && v != "" {
			modelID = v
		}
		mgr.RegisterAlias(alias, modelID)
	}

	models := make(map[string]llms.Model, len(defaults))
	for alias := range defaults {
		name := alias
		if alias == mainAlias && mainOverride != "" {
			name = mainOverride
		}
		m, err := mgr.GetModel(ctx, name)
		if err != nil {
			return nil, errors.Wrapf(err, "resolve model %q", name)
		}
		models[alias] = m
	}
	return models, nil
}
