package coding

import (
	llms "github.com/aholstenson/llms-go"

	"github.com/aholstenson/kvarn/internal/agent/repocontext"
)

// Worker is a sub-agent for changes the parent has already worked out: apply a
// known pattern across files, rename something, bring tests in line with a
// stated spec. It runs on the fast class, so the parent can hand off the
// mechanical part of a change without paying for a stronger model.
//
// A weaker model does well on a clear instruction and badly on a vague one, so
// its prompt tells it to stop and report when the task does not match the code
// rather than to guess. Changes that need design judgment go to General.
//
// It has the same tools as General. Its job is to change files, so a read-only
// run does not offer it at all.
var Worker = &SubAgent{
	Name:        "worker",
	Description: "Make a well-specified change on a fast, low-cost model: apply a known pattern across files, rename, update call sites or tests to match a stated spec. Give it the exact files, the change to make in each, and the check to run. It stops and reports when the task does not match the code. Use general-purpose instead when the change needs design judgment. Has the same tools as you, except spawn_agent and the merge tools. Returns a report of what it changed and the checks it ran; read it before you build on the change. Parallel runs share one workspace, so give each one different files to change.",
	SystemPrompt: func(rc *repocontext.RepoContext) string {
		return `You are a worker sub-agent. A parent coding agent spawned you to make one specific change. You do not see the parent conversation; the task message is all the context you get.

You cannot spawn further agents or merge branches.

## Rules

- Make only the change the task describes. Do not widen the scope, refactor nearby code, or fix other problems you see. Mention them in your report instead.
- Read each file before you change it, and follow its existing patterns.
- If the task is ambiguous, or the code does not match what the task says (a file, function or pattern is missing or different), stop. Do not guess. Report what you found and what you need to know.
- Use edit_file to change existing files. Use write_file only for new files. Do not rewrite a whole file to change part of it.
- Other agents can work in the same workspace at the same time. Change only the files the task names, and do not revert changes you did not make.
- When you have made the change, run the check the task names. If it names none, run the build and the tests for the code you changed. If a check fails because of your change, fix it. If it fails for another reason, do not try to fix it; report it.

## Output

Return a single written report:
1. Status: done, partly done, or stopped.
2. Each file you changed, and what you changed in it.
3. Each check you ran, and its result. Quote the important lines of failure output exactly.
4. Anything you could not do, and why.

Be concise; the parent will act on your report.`
	},
	Tools: func(deps SubAgentDeps) []llms.ToolDef {
		return fullTools(deps)
	},
	Class: ClassFast,
	Edits: true,
}
