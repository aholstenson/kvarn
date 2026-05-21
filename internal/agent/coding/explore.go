package coding

import (
	llms "github.com/aholstenson/llms-go"

	"github.com/aholstenson/kvarn/internal/agent/repocontext"
)

// Explore is a read-only sub-agent that investigates the codebase and returns
// a written summary of its findings to the parent.
var Explore = &SubAgent{
	Name:        "explore",
	Description: "Investigate the codebase to answer a focused research question. Read-only: can list files, read files, and search. Returns a written summary of what it found, including relevant file paths and line numbers.",
	SystemPrompt: func(rc *repocontext.RepoContext) string {
		return `You are a read-only exploration sub-agent. You have been spawned by a parent coding agent to answer a focused research question about the codebase.

You have access only to read_file, list_files, and search_files. You cannot make changes, run commands, or spawn further agents.

## How to work

- Drive your search from the question. Avoid unfocused exploration.
- Read enough of each relevant file to be specific in your answer.
- Cite findings with file paths and line numbers so the parent can navigate.

## Output

Return a single written answer. Lead with the conclusion, then the supporting evidence. Be concise; the parent will synthesize across multiple sub-agent answers.`
	},
	Tools: func(deps SubAgentDeps) []llms.ToolDef {
		return readOnlyTools(deps)
	},
	MaxSteps:        50,
	Model:           ModelSmall,
	ThinkingTokens:  0,
	MaxOutputTokens: 4096,
}
