package model

import (
	"context"
	"slices"
	"strings"

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

// apply layers a user override onto an entry. A field the override does not
// mention keeps the value it already had, so a block that sets one key does not
// reset the rest of the alias to zero.
func (e Entry) apply(raw RawEntry) Entry {
	if raw.ModelID != "" {
		e.ModelID = raw.ModelID
	}
	if raw.ReasoningEffort != nil {
		e.ReasoningEffort = *raw.ReasoningEffort
	}
	if raw.MaxOutputTokens != nil {
		e.MaxOutputTokens = *raw.MaxOutputTokens
	}
	if raw.MaxSteps != nil {
		e.MaxSteps = *raw.MaxSteps
	}
	return e
}

// Store reads model-alias configuration from user config. An empty result
// (e.g. when the backing file does not exist) is not an error — callers are
// expected to layer overrides on top of built-in defaults.
type Store interface {
	All(ctx context.Context) (map[string]RawEntry, error)
}

// RawAgent is the user-supplied override for a single named agent. Class
// repoints the agent at a different model alias; the embedded RawEntry tunes
// individual settings on top of whichever alias ends up selected.
type RawAgent struct {
	Class string
	RawEntry
}

// AgentStore reads per-agent overrides from user config. As with Store, an
// empty result is not an error.
type AgentStore interface {
	Agents(ctx context.Context) (map[string]RawAgent, error)
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

	for alias := range overrides {
		if _, ok := defaults[alias]; !ok {
			return nil, nil, fmt.Errorf("unknown model alias %q; known aliases are %s", alias, names(defaults))
		}
	}

	resolved := make(map[string]Entry, len(defaults))
	for alias, def := range defaults {
		entry := def.apply(overrides[alias])
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

// ResolveAgents resolves one model per named agent. classes holds the alias
// entries Resolve produced; agents names every agent that can be spawned,
// mapped to the alias it runs on unless the user says otherwise.
//
// An agent is configured in two layers: it picks a class, and it may then tune
// individual settings on top of that class. Both layers live in the same
// [agents.<name>] block, so a user who only wants to move planning to a
// stronger tier writes one key and inherits the rest.
func ResolveAgents(
	ctx context.Context,
	mgr *llms.Manager,
	store AgentStore,
	classes map[string]Entry,
	agents map[string]string,
) (map[string]llms.Model, map[string]Entry, error) {
	overrides, err := store.Agents(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("load agent config: %w", err)
	}
	for name := range overrides {
		if _, ok := agents[name]; !ok {
			return nil, nil, fmt.Errorf("unknown agent %q; known agents are %s", name, names(agents))
		}
	}

	models := make(map[string]llms.Model, len(agents))
	resolved := make(map[string]Entry, len(agents))
	for name, class := range agents {
		raw, overridden := overrides[name]
		if raw.Class != "" {
			class = raw.Class
		}
		entry, ok := classes[class]
		if !ok {
			return nil, nil, fmt.Errorf("agent %q names unknown class %q; known classes are %s",
				name, class, names(classes))
		}
		if overridden {
			entry = entry.apply(raw.RawEntry)
		}

		// The class alias is registered with the manager, but an agent that
		// overrides `model` is not: resolve by model ID, which GetModel accepts
		// in the same position as an alias.
		m, err := mgr.GetModel(ctx, entry.ModelID)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve model for agent %q: %w", name, err)
		}
		models[name] = m
		resolved[name] = entry
	}
	return models, resolved, nil
}

// names lists a map's keys in sorted order, for error messages that tell the
// user what they could have written instead.
func names[V any](m map[string]V) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return strings.Join(ks, ", ")
}
