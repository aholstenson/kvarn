package coding

import (
	llms "github.com/aholstenson/llms-go"

	"github.com/aholstenson/kvarn/internal/agent/repocontext"
)

// General is a sub-agent for self-contained, multi-step work that does not fit
// a narrower sub-agent: a research question that needs commands, or a bounded
// change the parent wants done outside its own context.
//
// It gets the same tools as the parent, with three exceptions. It cannot spawn
// sub-agents, because nested runs would multiply cost with nothing to bound
// them. It cannot merge, because a merge is a run-level decision that only the
// parent can checkpoint. And in a read-only run it cannot edit files, because
// a sub-agent must never be a path around the parent's own restrictions.
var General = &SubAgent{
	Name:        "general-purpose",
	Description: "Do a self-contained, multi-step task: research that needs shell commands, or a bounded change. Has the same tools as you, except spawn_agent and the merge tools; it edits files only when this run may edit files. Returns a written report of what it found or changed. Parallel runs share one workspace, so give each one different files to change.",
	SystemPrompt: func(rc *repocontext.RepoContext) string {
		return `You are a general-purpose sub-agent. A parent coding agent spawned you to do one self-contained task. You do not see the parent conversation; the task message is all the context you get.

You cannot spawn further agents or merge branches. If edit_file and write_file are not in your tools, this run is read-only: investigate and report, and do not try to change files with shell commands.

## How to work

- Do the task you were given and nothing more. Do not widen the scope.
- Read the relevant code before you change it, and follow its existing patterns.
- Other agents can work in the same workspace at the same time. Change only the files the task needs, and do not revert changes you did not make.
- Run the checks that prove your work (build, tests, linters) when the task changes code.

## Output

Return a single written report. Start with the result, then give the evidence:
- For research: the answer, with file paths and line numbers.
- For changes: each file you changed and why, plus the checks you ran and their results.
- Anything you could not finish, and why.

Be concise; the parent will act on your report.`
	},
	Tools: func(deps SubAgentDeps) []llms.ToolDef {
		tk := &CodingToolkit{
			runner:     deps.Runner,
			workingDir: deps.WorkingDir,
			sessionID:  deps.SessionID,
			skills:     deps.Skills,
			tasks:      NewTaskList(),
		}
		// The toolkit has no sub-agents and no merge target, so neither
		// spawn_agent nor the merge tools are in the set it returns.
		if deps.ReadOnly {
			return tk.ReadOnlyTools()
		}
		return tk.Tools()
	},
	Class: ClassBalanced,
}
