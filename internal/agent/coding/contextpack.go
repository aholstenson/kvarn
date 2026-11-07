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
	// Attachments are the files the requester sent with this run. They are
	// listed after the task whatever the mode's context blocks are, because
	// they belong to the request, not to the pull request's history.
	Attachments []AttachmentRef
}

// AttachmentRef describes one file the requester sent, as the agent is told
// about it.
type AttachmentRef struct {
	Name      string
	MediaType string
	Size      int64
	// Path is where the file is in the VM, or empty when the file was not
	// written there.
	Path string
	// InPrompt is set when the file is also in the first message.
	InPrompt bool
}

// BuildPrompt assembles the task message for a run: the mode's context blocks
// in the order it declares them, then the requester's own message, then the
// list of attached files. A mode with no context blocks gets the message
// alone, which is what a fresh job sends.
func (m *Mode) BuildPrompt(in ContextInput) string {
	return m.buildContextPack(in) + attachmentsSection(in.Attachments)
}

// attachmentsSection lists the requester's files with where to find each one.
// The agent cannot discover them by looking at the repository, so a file left
// off this list is a file it does not know it has.
//
// A file that reached neither the VM nor the first message is left off: the
// agent has no way to get at it, so naming it would only send it looking.
func attachmentsSection(refs []AttachmentRef) string {
	var lines []string
	for _, a := range refs {
		desc := fmt.Sprintf("%s, %s", a.MediaType, formatBytes(a.Size))
		switch {
		case a.Path != "" && a.InPrompt:
			lines = append(lines, fmt.Sprintf("- %s (%s; also included in this message)", a.Path, desc))
		case a.Path != "":
			lines = append(lines, fmt.Sprintf("- %s (%s)", a.Path, desc))
		case a.InPrompt:
			lines = append(lines, fmt.Sprintf("- %s (%s; included in this message only, not in the VM)", a.Name, desc))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n\n## Attachments\n\n" +
		"The requester sent these files with the task. A file with a path is in the VM, outside the repository, so it is not part of your changes; " +
		"copy it into the repository only if the task needs it there. " +
		"read_file reads a text file by its path, and view_file shows an image or a PDF.\n\n" +
		strings.Join(lines, "\n") + "\n"
}

func (m *Mode) buildContextPack(in ContextInput) string {
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
