package coding

const (
	// ModelMain is the alias for the primary coding-agent model. The
	// top-level agent loop uses it, as do sub-agents that do not declare
	// their own model.
	ModelMain = "coding-agent"

	// ModelSmall is the alias for a cheaper, faster model used by read-only
	// sub-agents that don't need top-tier reasoning (e.g. Explore).
	ModelSmall = "coding-agent-small"
)

// DefaultModels returns the built-in alias → provider/model-id mapping used
// when models.toml does not override an entry.
func DefaultModels() map[string]string {
	return map[string]string{
		ModelMain:  "anthropic/claude-sonnet-4-6",
		ModelSmall: "anthropic/claude-haiku-4-5",
	}
}
