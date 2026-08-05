package coding

import (
	"fmt"
	"strings"
)

// ContextInput carries everything a context pack can be built from. The caller
// fills in whatever it could obtain; a block whose content is missing is left
// out rather than emitted empty, so a pruned parent session or a diff the forge
// declined to return costs a section and not the run.
type ContextInput struct {
	// OriginalTask is the prompt the pull request's first run was given.
	OriginalTask string
	// PRTitle and PRBody describe the pull request as it stands now.
	PRTitle string
	PRBody  string
	// PRDiff is the pull request's diff.
	PRDiff string
	// Task is what the requester asked for on this run. It always ends the
	// pack, under the mode's own heading.
	Task string
}

// BuildPrompt assembles the task message for a run: the mode's context blocks
// in the order it declares them, then the requester's own message. A mode with
// no context blocks gets the message alone, which is what a fresh job sends.
func (m *Mode) BuildPrompt(in ContextInput) string {
	task := strings.TrimSpace(in.Task)
	if len(m.Context) == 0 {
		return task
	}

	var sb strings.Builder
	for _, block := range m.Context {
		switch block {
		case ContextOriginalTask:
			if t := strings.TrimSpace(in.OriginalTask); t != "" {
				sb.WriteString("## Original task\n\n")
				sb.WriteString(t)
				sb.WriteString("\n\n")
			}
		case ContextPRMetadata:
			if title, body := strings.TrimSpace(in.PRTitle), strings.TrimSpace(in.PRBody); title != "" || body != "" {
				sb.WriteString("## Current pull request\n\n")
				fmt.Fprintf(&sb, "%s\n", title)
				if body != "" {
					sb.WriteString("\n")
					sb.WriteString(body)
					sb.WriteString("\n")
				}
			}
		case ContextPRDiff:
			if d := strings.TrimSpace(in.PRDiff); d != "" {
				sb.WriteString("\n## Diff\n\n```diff\n")
				sb.WriteString(d)
				sb.WriteString("\n```\n")
			}
		}
	}

	if sb.Len() == 0 {
		return task
	}
	fmt.Fprintf(&sb, "\n## %s\n\n", m.TaskHeading())
	sb.WriteString(task)
	return sb.String()
}
