package coding

import (
	"fmt"
	"strings"

	"github.com/aholstenson/kvarn/internal/agent/repocontext"
)

// Mode is the high-level operating mode of a coding-agent run. It owns the
// top-level system prompt so new modes (review, research, ...) can be added
// without forking the agent loop.
type Mode struct {
	Name         string
	SystemPrompt func(projectName, repoURL, branch string, rc *repocontext.RepoContext, subAgents SubAgents) string
}

// ModeName satisfies the agent.Mode interface.
func (m *Mode) ModeName() string { return m.Name }

// ModeImplement is the default mode: the agent is told to implement or fix
// what the task message describes and verify with the project's own commands.
var ModeImplement = &Mode{
	Name:         "implement",
	SystemPrompt: buildImplementPrompt,
}

func buildImplementPrompt(projectName, repoURL, branch string, rc *repocontext.RepoContext, subAgents SubAgents) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, `You are an autonomous coding agent running in a sandboxed VM. There is no interactive user. You receive a single task message (separate from this system prompt) that may include requirements, specifications, error output, failing tests, or partial context. Implement or fix what that message describes and verify your work with the project's own commands.

## Environment

- Project: %s
- Repository: %s
- Branch: %s
- Working directory: /home/kvarn/workspace (the repository is cloned here)

## Task message as source of truth

- Treat the task message as authoritative. If it conflicts with assumptions, follow the task message and align the codebase to it.
- If something essential is missing, infer from the repository (existing patterns, tests, config) and prefer the smallest reasonable interpretation. Do not ask questions.

## Workflow

1. Orient with list_files and search_files only as needed; avoid unfocused exploration.
2. Before editing, read the file with read_file. The response gives each line a short hash; reference those hashes (not the line text) in your edit_file calls, and pass the version field back as expected_version.
3. Prefer edit_file for existing files; use write_file only for new files (or to overwrite an entire existing file with an explicit expected_version).
4. If edit_file fails with a version_conflict or anchor_mismatch, re-read the file to get fresh anchors before retrying — never reproduce line text by memory.
5. After substantive edits, run the project's build and tests (or the commands implied by the repo or task message).
6. On failure, use the actual error output to drive the next fix. Repeat until green or until blocked by a clear external constraint; if blocked, state that briefly in your final summary.

## Quality

- Match existing style, structure, and tooling.
- Keep changes minimal and scoped to the task. Do not refactor unrelated code, rename APIs, or clean up beyond what the task requires unless necessary to complete it.
- Do not disable tests, weaken assertions, or paper over failures unless the task explicitly allows it.

## Output

- After completing all work and verifying it passes, provide a clear summary of what you changed and why.
- This summary will be used for the commit message and pull request description, so be specific about what changed.`, projectName, repoURL, branch)

	if rc != nil {
		if rc.Instructions != "" {
			sb.WriteString("\n\n## Project Instructions (AGENTS.md / CLAUDE.md)\n\n")
			sb.WriteString(rc.Instructions)
		}

		if len(rc.Skills) > 0 {
			sb.WriteString("\n\n## Available Skills\n\n")
			sb.WriteString("The following skills provide specialized instructions for specific tasks.\n")
			sb.WriteString("When a task matches a skill's description, call the activate_skill tool\n")
			sb.WriteString("with the skill's name to load its full instructions.\n\n")
			sb.WriteString("<available_skills>\n")
			for _, skill := range rc.Skills {
				fmt.Fprintf(&sb, "  <skill>\n    <name>%s</name>\n    <description>%s</description>\n  </skill>\n", skill.Name, skill.Description)
			}
			sb.WriteString("</available_skills>")
		}
	}

	if len(subAgents) > 0 {
		sb.WriteString("\n\n## Available Sub-Agents\n\n")
		sb.WriteString("You can delegate focused work to a sub-agent by calling the spawn_agent\n")
		sb.WriteString("tool. Each sub-agent runs its own LLM loop with a restricted toolset and\n")
		sb.WriteString("returns a written answer. When you issue multiple spawn_agent calls in\n")
		sb.WriteString("the same turn they run in parallel; use this to keep your own context\n")
		sb.WriteString("focused on synthesis and edits.\n\n")
		sb.WriteString("<available_sub_agents>\n")
		for _, sub := range subAgents {
			fmt.Fprintf(&sb, "  <sub_agent>\n    <name>%s</name>\n    <description>%s</description>\n  </sub_agent>\n", sub.Name, sub.Description)
		}
		sb.WriteString("</available_sub_agents>")
	}

	return sb.String()
}
