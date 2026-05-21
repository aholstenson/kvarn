package coding

import modelcfg "github.com/aholstenson/kvarn/internal/config/model"

const (
	// ModelMain is the alias for the primary coding-agent model used by
	// the top-level agent loop.
	ModelMain = "coding-agent"

	// ModelSmall is the alias for a cheaper, faster model used by sub-agents
	// that do not need top-tier reasoning (e.g. Explore).
	ModelSmall = "coding-agent-small"
)

// DefaultModels returns the built-in alias configuration used when agents.toml
// does not override an entry.
func DefaultModels() map[string]modelcfg.Entry {
	return map[string]modelcfg.Entry{
		ModelMain: {
			ModelID:         "anthropic/claude-sonnet-4-6",
			ThinkingTokens:  10000,
			MaxOutputTokens: 16384,
			MaxSteps:        100,
		},
		ModelSmall: {
			ModelID:         "anthropic/claude-haiku-4-5",
			MaxOutputTokens: 8192,
			MaxSteps:        50,
		},
	}
}
