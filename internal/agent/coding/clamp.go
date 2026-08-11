package coding

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Every tool result the model reads is bounded, because a tool result is not
// something the conversation can take back. It is appended to the message
// history and replayed on every subsequent step, so a single unbounded result —
// a grep that wandered into .git, a `cat` of a build artifact — does not cost
// one call, it costs the rest of the run, and once it exceeds the model's
// context window there is no way to remove it and continue.
//
// The caps are deliberately far below any context window. A result at the cap
// is already more than a step should be spending; the point of the ceiling is
// that nothing can climb past it, not that reaching it is fine.
const (
	// maxToolResultBytes bounds command output and directory listings, at
	// roughly eight thousand tokens.
	maxToolResultBytes = 32 * 1024
	// maxFileToolResultBytes bounds results that are meant to be a faithful
	// copy of something — a file's contents, a skill's instructions, a
	// sub-agent's report. Cutting these mid-way costs more than cutting a log,
	// so they get more room before the ceiling applies.
	maxFileToolResultBytes = 128 * 1024
)

// lineSnapWindow is how far the clamp looks for a newline before giving up and
// cutting mid-line. Wide enough for a long source line, narrow enough that
// output with no newlines at all (a minified bundle, a base64 blob) still gets
// cut close to the budget.
const lineSnapWindow = 4 * 1024

// resultLimit is the ceiling for one tool's rendered result, plus the advice
// the model is given when the ceiling is hit. The advice matters as much as
// the cut: a model told only that output was dropped will re-run the same call
// and hit the same wall, while one told how to ask a narrower question moves on.
type resultLimit struct {
	bytes int
	hint  string
}

// resultLimits holds the per-tool ceilings. Tools absent from the map get
// defaultResultLimit, so a tool added later is bounded whether or not whoever
// adds it thinks about this file.
var resultLimits = map[string]resultLimit{
	"exec_command": {maxToolResultBytes, "re-run with the output narrowed (a grep, a tail, a quieter flag) if you need the rest"},
	"search_files": {maxToolResultBytes, "search a narrower path or glob, or use a more specific pattern"},
	"list_files":   {maxToolResultBytes, "list a subdirectory, or lower max_depth"},
	"read_file":    {maxFileToolResultBytes, "read a window of the file with start_line and end_line"},
	"edit_file":    {maxFileToolResultBytes, "read a window of the file with start_line and end_line"},
	"write_file":   {maxFileToolResultBytes, "read a window of the file with start_line and end_line"},

	"activate_skill": {maxFileToolResultBytes, ""},
	"spawn_agent":    {maxFileToolResultBytes, "ask the sub-agent for a shorter answer"},
}

var defaultResultLimit = resultLimit{bytes: maxToolResultBytes}

func limitForTool(name string) resultLimit {
	if l, ok := resultLimits[name]; ok {
		return l
	}
	return defaultResultLimit
}

// clampToolText cuts text down to limit bytes, keeping its start and its end
// and dropping the middle. Both ends are kept for the same reason the runner
// keeps both: the head says what was asked and the tail says how it came out.
//
// The cut lands on a line boundary when one is close enough, and never inside
// a UTF-8 rune.
func clampToolText(text string, limit int, hint string) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}

	headBudget := limit / 2
	tailBudget := limit - headBudget

	head := text[:headBudget]
	if i := strings.LastIndexByte(head, '\n'); i >= 0 && len(head)-i <= lineSnapWindow {
		head = head[:i+1]
	}
	head = trimPartialRuneSuffix(head)

	tail := text[len(text)-tailBudget:]
	if i := strings.IndexByte(tail, '\n'); i >= 0 && i < lineSnapWindow {
		tail = tail[i+1:]
	}
	tail = trimPartialRunePrefix(tail)

	dropped := len(text) - len(head) - len(tail)
	var marker strings.Builder
	fmt.Fprintf(&marker, "\n…[kvarn: %s of this result omitted", formatBytes(int64(dropped)))
	if hint != "" {
		marker.WriteString("; ")
		marker.WriteString(hint)
	}
	marker.WriteString("]…\n")

	return head + marker.String() + tail
}

// trimPartialRuneSuffix drops a multi-byte rune left half-written by a cut.
func trimPartialRuneSuffix(s string) string {
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size > 1 {
			return s
		}
		s = s[:len(s)-1]
	}
	return s
}

// trimPartialRunePrefix drops the trailing bytes of a rune whose first bytes
// fell on the other side of a cut.
func trimPartialRunePrefix(s string) string {
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		if r != utf8.RuneError || size > 1 {
			return s
		}
		s = s[1:]
	}
	return s
}

// formatBytes renders a byte count for a truncation marker.
func formatBytes(b int64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case b >= gib:
		return fmt.Sprintf("%.1fG", float64(b)/float64(gib))
	case b >= mib:
		return fmt.Sprintf("%.1fM", float64(b)/float64(mib))
	case b >= kib:
		return fmt.Sprintf("%.1fK", float64(b)/float64(kib))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
