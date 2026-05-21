package coding

import (
	llms "github.com/aholstenson/llms-go"

	"github.com/aholstenson/kvarn/internal/agent/repocontext"
)

// Plan is a read-only sub-agent that produces an implementation plan for a
// task without making any changes.
var Plan = &SubAgent{
	Name:        "plan",
	Description: "Produce a step-by-step implementation plan for a task. Read-only: can list files, read files, and search. Returns a plan describing which files to change, what to add or remove, and the order of work.",
	SystemPrompt: func(rc *repocontext.RepoContext) string {
		return `You are a read-only planning sub-agent. You have been spawned by a parent coding agent to design an implementation strategy for a task.

You have access only to read_file, list_files, and search_files. You cannot make changes, run commands, or spawn further agents.

## How to work

- Understand the task and the relevant parts of the codebase before planning.
- Identify the files that need to change and what each change should look like.
- Call out architectural trade-offs and risks the parent should weigh.

## Output

Return a written plan with:
1. A short statement of the goal and constraints.
2. A numbered list of steps, each naming the file(s) involved and the intent of the change.
3. Open questions or risks the parent should consider before starting.

Be specific about file paths and the smallest reasonable scope. Do not produce diffs.`
	},
	Tools: func(deps SubAgentDeps) []llms.ToolDef {
		return readOnlyTools(deps)
	},
	MaxSteps:        50,
	Model:           ModelMain,
	ThinkingTokens:  5000,
	MaxOutputTokens: 8192,
}
