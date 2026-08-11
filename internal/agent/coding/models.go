package coding

import (
	"context"
	"log/slog"
	"sync"

	llms "github.com/aholstenson/llms-go"

	modelcfg "github.com/aholstenson/kvarn/internal/config/model"
)

// Class names a capability tier an agent can run on. Every agent — the main
// loop and each sub-agent — picks a class rather than a model, so a user who
// wants planning done by a stronger model repoints one tier instead of hunting
// down every caller.
//
// A class is spelled the same as the model alias backing it, which is what lets
// `class = "coding-agent-reasoning"` in an [agents.<name>] block and the
// [models.coding-agent-reasoning] block that configures it be read as the same
// thing.
type Class string

const (
	// ClassFast is for high-volume, shallow work where breadth of search
	// matters more than depth of reasoning.
	ClassFast Class = "coding-agent-fast"
	// ClassBalanced is the default tier: the main agent loop and anything that
	// needs no more than it.
	ClassBalanced Class = "coding-agent"
	// ClassReasoning is for work that is worth thinking hard about and is run
	// rarely enough to pay for it — designing a change before making it.
	ClassReasoning Class = "coding-agent-reasoning"
)

// ModelMain is the alias of the class the top-level agent loop runs on. It is
// also the alias `--model` repoints.
const ModelMain = string(ClassBalanced)

// DefaultModels returns the built-in class configuration used when agents.toml
// does not override an entry.
//
// The step budgets differ by tier because the tiers are used differently: a
// fast agent searches, which is many cheap steps, while a reasoning agent
// deliberates over a handful.
func DefaultModels() map[string]modelcfg.Entry {
	return map[string]modelcfg.Entry{
		string(ClassBalanced): {
			ModelID:         "anthropic/claude-sonnet-4-6",
			ReasoningEffort: llms.EffortMedium,
			MaxOutputTokens: 16384,
			MaxSteps:        100,
		},
		string(ClassFast): {
			ModelID:         "anthropic/claude-haiku-4-5",
			ReasoningEffort: llms.EffortNone,
			MaxOutputTokens: 8192,
			MaxSteps:        100,
		},
		string(ClassReasoning): {
			ModelID:         "anthropic/claude-sonnet-4-6",
			ReasoningEffort: llms.EffortHigh,
			MaxOutputTokens: 16384,
			MaxSteps:        50,
		},
	}
}

// Models is the resolved model set a coding agent runs with: one entry per
// class for the main loop, and one per sub-agent, already reduced from the
// class it named plus whatever the user layered on top.
type Models struct {
	// Classes maps a class alias to the model resolved for that tier.
	Classes map[string]llms.Model
	// ClassConfigs maps a class alias to its resolved settings.
	ClassConfigs map[string]modelcfg.Entry
	// Agents maps a sub-agent name to the model it runs on.
	Agents map[string]llms.Model
	// AgentConfigs maps a sub-agent name to its resolved settings.
	AgentConfigs map[string]modelcfg.Entry
}

// ModelStore is the agents.toml view the resolver needs: the class blocks and
// the per-agent blocks.
type ModelStore interface {
	modelcfg.Store
	modelcfg.AgentStore
}

// ResolveModels resolves everything a coding agent needs from agents.toml.
// mainOverride, when non-empty, picks the model the balanced class resolves to,
// which is what `--model` sets.
func ResolveModels(ctx context.Context, mgr *llms.Manager, store ModelStore, mainOverride string) (Models, error) {
	classes, classCfgs, err := modelcfg.Resolve(ctx, mgr, store, DefaultModels(), ModelMain, mainOverride)
	if err != nil {
		return Models{}, err
	}
	agents, agentCfgs, err := modelcfg.ResolveAgents(ctx, mgr, store, classCfgs, DefaultAgentClasses())
	if err != nil {
		return Models{}, err
	}
	return Models{
		Classes:      classes,
		ClassConfigs: classCfgs,
		Agents:       agents,
		AgentConfigs: agentCfgs,
	}, nil
}

// Resolver produces the model set for a single job, re-reading agents.toml
// every time so that an edit to a [models.*] or [agents.*] block reaches the
// next job without restarting the orchestrator. This matches how the rest of
// the file already behaves — [defaults] is read per job — and how an operator
// reasonably expects a hand-edited config file to work.
//
// Resolutions are serialized. Resolving registers each class alias on the
// shared llms.Manager and then reads models back out through that registry, so
// two jobs resolving concurrently must not interleave those two halves and see
// each other's aliases.
//
// A resolution that fails falls back to the last one that succeeded. agents.toml
// is hand-edited while the orchestrator is running, and a file caught
// mid-save — or a typo — should cost the operator a warning rather than every
// job that happens to start before they fix it.
type Resolver struct {
	mgr          *llms.Manager
	store        ModelStore
	mainOverride string

	mu       sync.Mutex
	last     Models
	haveLast bool
}

// NewResolver creates a Resolver reading from store. mainOverride, when
// non-empty, picks the model the balanced class resolves to, which is what
// `--model` sets.
func NewResolver(mgr *llms.Manager, store ModelStore, mainOverride string) *Resolver {
	return &Resolver{mgr: mgr, store: store, mainOverride: mainOverride}
}

// Resolve returns the model set a job should run with. The first call has no
// previous set to fall back on, so it reports configuration errors to the
// caller; callers resolve once at startup for exactly that reason.
func (r *Resolver) Resolve(ctx context.Context) (Models, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	models, err := ResolveModels(ctx, r.mgr, r.store, r.mainOverride)
	if err != nil {
		if !r.haveLast {
			return Models{}, err
		}
		slog.Warn("failed to reload agent model config; running with the last configuration that loaded", "error", err)
		return r.last, nil
	}
	r.last = models
	r.haveLast = true
	return models, nil
}

// DefaultAgentClasses maps each sub-agent to the class it runs on unless an
// [agents.<name>] block repoints it.
func DefaultAgentClasses() map[string]string {
	out := make(map[string]string, len(BuiltinSubAgents()))
	for name, sub := range BuiltinSubAgents() {
		out[name] = string(sub.Class)
	}
	return out
}
