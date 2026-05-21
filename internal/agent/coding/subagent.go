package coding

import (
	llms "github.com/aholstenson/llms-go"

	"github.com/aholstenson/kvarn/internal/agent/repocontext"
	"github.com/aholstenson/kvarn/internal/sandbox"
)

// SubAgent is a named, specialized agent the parent coding agent can spawn via
// the spawn_agent tool. Each sub-agent runs an independent LLM loop with its
// own system prompt, toolset, and shell session, and returns only its final
// text to the parent.
type SubAgent struct {
	// Name identifies the sub-agent in the spawn_agent tool call (e.g. "explore").
	Name string
	// Description is shown to the parent so it knows when to spawn this agent.
	Description string
	// SystemPrompt builds the system prompt for a sub-agent run.
	SystemPrompt func(rc *repocontext.RepoContext) string
	// Tools constructs the sub-agent's toolkit from the run-scoped dependencies.
	Tools func(deps SubAgentDeps) []llms.ToolDef
	// MaxSteps caps how many tool-call steps the sub-agent may take.
	MaxSteps int
	// Model is the alias of the LLM the sub-agent runs against. Empty falls
	// back to ModelMain.
	Model string
	// ThinkingTokens is the thinking budget for sub-agent calls. 0 disables
	// thinking.
	ThinkingTokens int
	// MaxOutputTokens caps the sub-agent's output. 0 falls back to 16384.
	MaxOutputTokens int
}

// SubAgentDeps carries everything a sub-agent's toolkit needs to construct
// itself for a single run.
type SubAgentDeps struct {
	Runner     sandbox.RunnerProxy
	WorkingDir string
	SessionID  string
	Skills     map[string]*repocontext.Skill
}

// SubAgents holds the registered sub-agent types keyed by Name.
type SubAgents map[string]*SubAgent

// readOnlyTools returns the read-only tools shared by Explore and Plan.
func readOnlyTools(deps SubAgentDeps) []llms.ToolDef {
	tk := &CodingToolkit{
		runner:     deps.Runner,
		workingDir: deps.WorkingDir,
		sessionID:  deps.SessionID,
		skills:     deps.Skills,
		tasks:      NewTaskList(),
	}
	return []llms.ToolDef{
		llms.NewToolDef(&readFileTool{toolkit: tk}),
		llms.NewToolDef(&listFilesTool{toolkit: tk}),
		llms.NewToolDef(&searchFilesTool{toolkit: tk}),
	}
}
