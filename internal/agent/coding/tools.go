package coding

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	llms "github.com/aholstenson/llms-go"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/agent/cost"
	"github.com/aholstenson/kvarn/internal/agent/repocontext"
	modelcfg "github.com/aholstenson/kvarn/internal/config/model"
	"github.com/aholstenson/kvarn/internal/sandbox"
)

// CodingToolkit provides file manipulation and shell tools for the coding agent.
type CodingToolkit struct {
	runner       sandbox.RunnerProxy
	workingDir   string
	sessionID    string
	skills       map[string]*repocontext.Skill
	agentModels  map[string]llms.Model
	agentConfigs map[string]modelcfg.Entry
	subAgents    SubAgents
	repoCtx    *repocontext.RepoContext
	tracker    *cost.Tracker
	tasks      *TaskList
}

// CodingToolkitOpts configures a CodingToolkit. AgentModels, SubAgents, and
// RepoCtx are required when the parent agent should be able to spawn
// sub-agents; the spawn_agent tool is omitted from the toolkit when SubAgents
// is empty or no sub-agent models are available.
type CodingToolkitOpts struct {
	Runner     sandbox.RunnerProxy
	WorkingDir string
	SessionID  string
	Skills     []repocontext.Skill
	// AgentModels maps a sub-agent name to the model resolved for it.
	AgentModels map[string]llms.Model
	// AgentConfigs maps a sub-agent name to its resolved settings (reasoning
	// effort, output cap, step budget). May be nil if sub-agents are not used.
	AgentConfigs map[string]modelcfg.Entry
	SubAgents    SubAgents
	RepoCtx      *repocontext.RepoContext
	// Tracker, when set, gates tool results so the agent receives a one-shot
	// budget warning note in the next tool result it sees after the warn
	// threshold is crossed.
	Tracker *cost.Tracker
}

func NewCodingToolkit(runner sandbox.RunnerProxy, workingDir string, sessionID string, skills []repocontext.Skill) *CodingToolkit {
	return NewCodingToolkitWithOpts(CodingToolkitOpts{
		Runner:     runner,
		WorkingDir: workingDir,
		SessionID:  sessionID,
		Skills:     skills,
	})
}

func NewCodingToolkitWithOpts(opts CodingToolkitOpts) *CodingToolkit {
	skillMap := make(map[string]*repocontext.Skill, len(opts.Skills))
	for i := range opts.Skills {
		skillMap[opts.Skills[i].Name] = &opts.Skills[i]
	}
	return &CodingToolkit{
		runner:       opts.Runner,
		workingDir:   opts.WorkingDir,
		sessionID:    opts.SessionID,
		skills:       skillMap,
		agentModels:  opts.AgentModels,
		agentConfigs: opts.AgentConfigs,
		subAgents:    opts.SubAgents,
		repoCtx:      opts.RepoCtx,
		tracker:      opts.Tracker,
		tasks:        NewTaskList(),
	}
}

// guard wraps each ToolDef so that every result passes the same two checks on
// its way to the model: it is cut down to the tool's ceiling, and — once the
// tracker's warn threshold is crossed — it carries a one-shot budget note.
//
// Wrapping the whole set in one place is the point. A ceiling that each tool
// had to remember to apply is a ceiling the next tool will be missing.
func (t *CodingToolkit) guard(tools []llms.ToolDef) []llms.ToolDef {
	wrapped := make([]llms.ToolDef, len(tools))
	for i, td := range tools {
		wrapped[i] = &guardedTool{inner: td, limit: limitForTool(td.Name()), tracker: t.tracker}
	}
	return wrapped
}

// guardedTool decorates a ToolDef so that Render clamps the result and
// optionally appends a budget warning. All other behavior is forwarded verbatim.
type guardedTool struct {
	inner   llms.ToolDef
	limit   resultLimit
	tracker *cost.Tracker
}

func (w *guardedTool) Name() string        { return w.inner.Name() }
func (w *guardedTool) Description() string { return w.inner.Description() }
func (w *guardedTool) Schema() any         { return w.inner.Schema() }
func (w *guardedTool) Execute(ctx context.Context, in any) (any, error) {
	return w.inner.Execute(ctx, in)
}

func (w *guardedTool) Render(out any) llms.ToolResult {
	res := w.inner.Render(out)
	res.Text = clampToolText(res.Text, w.limit.bytes, w.limit.hint)
	// The budget note is appended after the clamp so it cannot be the thing
	// that gets cut.
	if w.tracker != nil {
		if note, ok := w.tracker.ConsumeWarning(); ok {
			if res.Text != "" {
				res.Text += "\n\n"
			}
			res.Text += note
		}
	}
	return res
}

func (t *CodingToolkit) Tools() []llms.ToolDef {
	tools := []llms.ToolDef{
		llms.NewToolDef(&execCommandTool{toolkit: t}),
		llms.NewToolDef(&readFileTool{toolkit: t}),
		llms.NewToolDef(&editFileTool{toolkit: t}),
		llms.NewToolDef(&writeFileTool{toolkit: t}),
		llms.NewToolDef(&listFilesTool{toolkit: t}),
		llms.NewToolDef(&searchFilesTool{toolkit: t}),
		llms.NewToolDef(&addTaskTool{toolkit: t}),
		llms.NewToolDef(&updateTaskTool{toolkit: t}),
		llms.NewToolDef(&listTasksTool{toolkit: t}),
	}
	if len(t.skills) > 0 {
		tools = append(tools, llms.NewToolDef(&activateSkillTool{skills: t.skills}))
	}
	if len(t.agentModels) > 0 && len(t.subAgents) > 0 {
		tools = append(tools, llms.NewToolDef(&spawnAgentTool{toolkit: t}))
	}
	return t.guard(tools)
}

// ReadOnlyTools returns the same toolkit minus edit_file and write_file. Used
// by read-only modes (review, research) that may still need to run shell
// commands for inspection but must not modify files.
func (t *CodingToolkit) ReadOnlyTools() []llms.ToolDef {
	tools := []llms.ToolDef{
		llms.NewToolDef(&execCommandTool{toolkit: t}),
		llms.NewToolDef(&readFileTool{toolkit: t}),
		llms.NewToolDef(&listFilesTool{toolkit: t}),
		llms.NewToolDef(&searchFilesTool{toolkit: t}),
		llms.NewToolDef(&addTaskTool{toolkit: t}),
		llms.NewToolDef(&updateTaskTool{toolkit: t}),
		llms.NewToolDef(&listTasksTool{toolkit: t}),
	}
	if len(t.skills) > 0 {
		tools = append(tools, llms.NewToolDef(&activateSkillTool{skills: t.skills}))
	}
	if len(t.agentModels) > 0 && len(t.subAgents) > 0 {
		tools = append(tools, llms.NewToolDef(&spawnAgentTool{toolkit: t}))
	}
	return t.guard(tools)
}

// exec_command

type ExecCommandInput struct {
	Command string   `json:"command" jsonschema:"description=The command to run"`
	Args    []string `json:"args,omitempty" jsonschema:"description=Arguments for the command. If empty and command contains spaces or pipes it runs through sh -c"`
}

type ExecCommandOutput struct {
	ExitCode int32
	Stdout   string
	Stderr   string
}

type execCommandTool struct {
	toolkit *CodingToolkit
}

func (t *execCommandTool) Name() string { return "exec_command" }
func (t *execCommandTool) Description() string {
	return "Run a shell command (build, test, lint, install deps). If no args are provided and the command contains spaces or pipes, it runs through sh -c for shell expansion."
}
func (t *execCommandTool) Schema() *ExecCommandInput { return &ExecCommandInput{} }

func (t *execCommandTool) Execute(ctx context.Context, input *ExecCommandInput) (*ExecCommandOutput, error) {
	cmd := input.Command
	if len(input.Args) > 0 {
		// Build a shell command from command + args, quoting each argument.
		parts := []string{cmd}
		for _, a := range input.Args {
			parts = append(parts, shellQuote(a))
		}
		cmd = strings.Join(parts, " ")
	}

	// The tool's own ceiling is passed down so the runner drops the excess at
	// the source. Truncating only on render would still mean a multi-megabyte
	// stream buffered in the guest and pushed across the bridge to be thrown
	// away on arrival.
	//
	// The cap applies to stdout and stderr separately, so the full budget is
	// available to whichever stream the command actually used. A command that
	// floods both leaves the rendered pair over the ceiling and the clamp on
	// the way to the model trims it the rest of the way.
	resp, err := t.toolkit.runner.SessionExec(ctx, &v1.SessionExecRequest{
		SessionId:      t.toolkit.sessionID,
		Command:        cmd,
		MaxOutputBytes: uint32(limitForTool(t.Name()).bytes),
	}, nil)
	if err != nil {
		return nil, err
	}

	return &ExecCommandOutput{
		ExitCode: resp.ExitCode,
		Stdout:   resp.Stdout,
		Stderr:   resp.Stderr,
	}, nil
}

// shellQuote wraps s in single quotes for safe shell interpolation.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (t *execCommandTool) Render(o *ExecCommandOutput) llms.ToolResult {
	var sb strings.Builder
	if o.Stdout != "" {
		sb.WriteString(o.Stdout)
	}
	if o.Stderr != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("STDERR:\n")
		sb.WriteString(o.Stderr)
	}
	fmt.Fprintf(&sb, "\n[exit code: %d]", o.ExitCode)
	return llms.TextToolResult(sb.String())
}

// read_file

// defaultReadLines bounds a single read, and is both the window a read gets
// when it asks for no particular one and the longest window it can ask for.
//
// A file this long is one to navigate rather than to hold: the anchors of the
// window that was read stay valid, so continuing from where the last read
// stopped costs one more call, while a whole-file read of something generated
// costs the rest of the conversation.
const defaultReadLines = 2000

type ReadFileInput struct {
	Path      string `json:"path" jsonschema:"description=Path to the file. Relative to the workspace root; an absolute path inside the workspace also works."`
	StartLine int    `json:"start_line,omitempty" jsonschema:"description=1-indexed start line. Omit or set to 0 to start at the top of the file."`
	EndLine   int    `json:"end_line,omitempty" jsonschema:"description=1-indexed inclusive end line. Omit or set to 0 for the rest of the window."`
}

// TaggedLineView is the public representation of a TaggedLine in tool output.
type TaggedLineView struct {
	Line    int32
	Hash    string
	Content string
}

type ReadFileOutput struct {
	Version    string
	TotalLines int32
	Lines      []TaggedLineView
	Newline    string
}

type readFileTool struct {
	toolkit *CodingToolkit
}

func (t *readFileTool) Name() string { return "read_file" }
func (t *readFileTool) Description() string {
	return `Read a file. Each line is returned with a short word anchor you can reference in subsequent edit_file calls. Example output line:

  12:cedar|  return "world";

Anchors are deterministic, single-word labels of the line's content. When two distinct lines in the same file would otherwise share the same word, both get a short hex suffix (e.g. "cedar:f1"). Anchors stay valid for unchanged lines across edits — you only need to re-read if the lines you want to touch have changed.

The first line of the output is "version: <hash>" — you may pass that back as expected_version on edit_file as an advisory check, but it is not required.

A read returns at most 2000 lines. Files shorter than that come back whole; for longer ones the response says which lines you got and where to continue, and start_line / end_line (1-indexed, inclusive) read any other window. The version and total_lines always describe the whole file.`
}
func (t *readFileTool) Schema() *ReadFileInput { return &ReadFileInput{} }

func (t *readFileTool) Execute(ctx context.Context, input *ReadFileInput) (*ReadFileOutput, error) {
	start, end := readWindow(input.StartLine, input.EndLine)

	resp, err := t.toolkit.runner.ReadFile(ctx, &v1.ReadFileRequest{
		WorkingDir: t.toolkit.workingDir,
		Path:       input.Path,
		StartLine:  int32(start),
		EndLine:    int32(end),
	})
	if err != nil {
		return nil, err
	}

	out := &ReadFileOutput{
		Version:    resp.Version,
		TotalLines: resp.TotalLines,
		Newline:    resp.Newline,
		Lines:      make([]TaggedLineView, len(resp.Lines)),
	}
	for i, l := range resp.Lines {
		out.Lines[i] = TaggedLineView{Line: l.Line, Hash: l.Hash, Content: l.Content}
	}
	return out, nil
}

// readWindow resolves the window a read actually asks the runner for. An
// unspecified start is the top of the file, and an unspecified or over-long end
// is defaultReadLines further on — so a read of an unfamiliar file is bounded
// whether or not the caller thought about how long it might be.
func readWindow(startLine, endLine int) (start, end int) {
	start = startLine
	if start <= 0 {
		start = 1
	}
	end = endLine
	if last := start + defaultReadLines - 1; end <= 0 || end > last {
		end = last
	}
	return start, end
}

func (t *readFileTool) Render(o *ReadFileOutput) llms.ToolResult {
	var sb strings.Builder
	fmt.Fprintf(&sb, "version: %s\n", o.Version)
	fmt.Fprintf(&sb, "total_lines: %d\n", o.TotalLines)
	for _, l := range o.Lines {
		fmt.Fprintf(&sb, "%d:%s|%s\n", l.Line, l.Hash, l.Content)
	}

	// A window that does not cover the file says so, and says where to pick the
	// file up again — a read that silently stops short is one the model has no
	// reason to continue.
	if len(o.Lines) > 0 {
		first := o.Lines[0].Line
		last := o.Lines[len(o.Lines)-1].Line
		if first > 1 || last < o.TotalLines {
			fmt.Fprintf(&sb, "\n[kvarn: showing lines %d-%d of %d.", first, last, o.TotalLines)
			if last < o.TotalLines {
				fmt.Fprintf(&sb, " Continue with start_line=%d.", last+1)
			}
			sb.WriteString("]\n")
		}
	}
	return llms.TextToolResult(sb.String())
}

// edit_file

// EditOperationInput is a flat representation of a single anchor-resolved edit
// operation. Which fields are required depends on Op:
//   - "replace" / "delete":                       hash (+ lines for replace)
//   - "insert_after" / "insert_before":           hash, lines
//   - "replace_range" / "delete_range":           start_hash, end_hash (+ lines for replace_range)
//
// The optional line / start_line / end_line are tiebreakers: supply them only
// when the same anchor matches multiple identical lines in the file.
type EditOperationInput struct {
	Op        string   `json:"op" jsonschema:"description=One of: replace, replace_range, insert_after, insert_before, delete, delete_range"`
	Line      int      `json:"line,omitempty" jsonschema:"description=Optional 1-indexed line tiebreaker for single-line ops. Needed only when the anchor matches multiple identical lines."`
	Hash      string   `json:"hash,omitempty" jsonschema:"description=Anchor from read_file identifying the target line. Required for all single-line ops."`
	StartLine int      `json:"start_line,omitempty" jsonschema:"description=Optional 1-indexed tiebreaker for the start of a range op."`
	StartHash string   `json:"start_hash,omitempty" jsonschema:"description=Anchor for the inclusive start of a range op."`
	EndLine   int      `json:"end_line,omitempty" jsonschema:"description=Optional 1-indexed tiebreaker for the end of a range op."`
	EndHash   string   `json:"end_hash,omitempty" jsonschema:"description=Anchor for the inclusive end of a range op."`
	Lines     []string `json:"lines,omitempty" jsonschema:"description=Replacement or insertion content. One entry per line, no trailing newlines."`
}

type EditFileInput struct {
	Path            string               `json:"path" jsonschema:"description=Path to the file. Relative to the workspace root; an absolute path inside the workspace also works."`
	ExpectedVersion string               `json:"expected_version,omitempty" jsonschema:"description=Optional/advisory version from the most recent read_file. If supplied and stale the edit still applies when every anchor resolves, and version_drift is reported."`
	Operations      []EditOperationInput `json:"operations" jsonschema:"description=Ordered list of anchor-resolved edits to apply atomically"`
	ContextLines    int                  `json:"context_lines,omitempty" jsonschema:"description=Fresh tagged context lines around each edit in the response. Default 5."`
}

type EditFileOutput struct {
	Version      string
	TotalLines   int32
	Context      []TaggedLineView
	VersionDrift bool
	// Failure carries a structured error message and (when relevant) a fresh
	// tagged snapshot of the file so the model can re-anchor.
	Failure  string
	Snapshot []TaggedLineView
}

type editFileTool struct {
	toolkit *CodingToolkit
}

func (t *editFileTool) Name() string { return "edit_file" }
func (t *editFileTool) Description() string {
	return `Apply anchor-resolved edits to a file transactionally. Each operation references the target line by its anchor from read_file. Line numbers are optional tiebreakers and are not normally needed.

Example operation:

  {"op": "replace", "hash": "cedar", "lines": ["  return \"hello world\";"]}

Supported ops:
- replace        — replace a single line (hash, lines)
- replace_range  — replace an inclusive line range (start_hash, end_hash, lines)
- insert_after   — insert new lines immediately after the given line (hash, lines)
- insert_before  — insert new lines immediately before the given line (hash, lines)
- delete         — delete a single line (hash)
- delete_range   — delete an inclusive line range (start_hash, end_hash)

Chained edits: anchors for unchanged lines stay valid across edits, so you can issue multiple edit_file calls without re-reading the file as long as you reference lines that haven't moved. expected_version is optional/advisory — if you supply it and the file changed elsewhere the edit still applies (as long as anchors resolve) and version_drift is reported. The line / start_line / end_line fields are tiebreakers: supply them only when the same anchor would match multiple identical lines elsewhere in the file.

On anchor mismatch the call fails atomically (nothing is applied) and returns a fresh tagged read so you can re-anchor in one round-trip.`
}
func (t *editFileTool) Schema() *EditFileInput { return &EditFileInput{} }

func (t *editFileTool) Execute(ctx context.Context, input *EditFileInput) (*EditFileOutput, error) {
	ops := make([]*v1.EditOperation, len(input.Operations))
	for i, op := range input.Operations {
		code, err := parseEditOp(op.Op)
		if err != nil {
			return nil, err
		}
		ops[i] = &v1.EditOperation{
			Op:        code,
			Line:      int32(op.Line),
			Hash:      op.Hash,
			StartLine: int32(op.StartLine),
			StartHash: op.StartHash,
			EndLine:   int32(op.EndLine),
			EndHash:   op.EndHash,
			Lines:     op.Lines,
		}
	}

	resp, err := t.toolkit.runner.EditFile(ctx, &v1.EditFileRequest{
		WorkingDir:      t.toolkit.workingDir,
		Path:            input.Path,
		ExpectedVersion: input.ExpectedVersion,
		Operations:      ops,
		ContextLines:    int32(input.ContextLines),
	})
	if err != nil {
		out := &EditFileOutput{Failure: err.Error()}
		if snap := extractSnapshot(err); snap != nil {
			out.Snapshot = make([]TaggedLineView, len(snap.Lines))
			for i, l := range snap.Lines {
				out.Snapshot[i] = TaggedLineView{Line: l.Line, Hash: l.Hash, Content: l.Content}
			}
			out.Version = snap.Version
			out.TotalLines = snap.TotalLines
		}
		return out, nil
	}

	out := &EditFileOutput{
		Version:      resp.Version,
		TotalLines:   resp.TotalLines,
		VersionDrift: resp.VersionDrift,
		Context:      make([]TaggedLineView, len(resp.Context)),
	}
	for i, l := range resp.Context {
		out.Context[i] = TaggedLineView{Line: l.Line, Hash: l.Hash, Content: l.Content}
	}
	return out, nil
}

func (t *editFileTool) Render(o *EditFileOutput) llms.ToolResult {
	if o.Failure != "" {
		var sb strings.Builder
		sb.WriteString("edit_file failed: ")
		sb.WriteString(o.Failure)
		sb.WriteString("\nRe-read the file to get fresh anchors before retrying.")
		if len(o.Snapshot) > 0 {
			fmt.Fprintf(&sb, "\nfresh version: %s\ntotal_lines: %d\n", o.Version, o.TotalLines)
			for _, l := range o.Snapshot {
				fmt.Fprintf(&sb, "%d:%s|%s\n", l.Line, l.Hash, l.Content)
			}
		}
		return llms.TextToolResult(sb.String())
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Edit applied. New version: %s (%d lines total)\n", o.Version, o.TotalLines)
	if o.VersionDrift {
		sb.WriteString("Note: the file changed elsewhere since your last read. Distant anchors may be stale.\n")
	}
	for _, l := range o.Context {
		fmt.Fprintf(&sb, "%d:%s|%s\n", l.Line, l.Hash, l.Content)
	}
	return llms.TextToolResult(sb.String())
}

func parseEditOp(s string) (v1.EditOp, error) {
	switch strings.ToLower(s) {
	case "replace":
		return v1.EditOp_EDIT_OP_REPLACE, nil
	case "replace_range":
		return v1.EditOp_EDIT_OP_REPLACE_RANGE, nil
	case "insert_after":
		return v1.EditOp_EDIT_OP_INSERT_AFTER, nil
	case "insert_before":
		return v1.EditOp_EDIT_OP_INSERT_BEFORE, nil
	case "delete":
		return v1.EditOp_EDIT_OP_DELETE, nil
	case "delete_range":
		return v1.EditOp_EDIT_OP_DELETE_RANGE, nil
	}
	return v1.EditOp_EDIT_OP_UNSPECIFIED, fmt.Errorf("unknown edit op %q", s)
}

// extractSnapshot pulls a ReadFileResponse out of a connect error's details, if
// present. The runner attaches one to version_conflict / anchor_mismatch.
func extractSnapshot(err error) *v1.ReadFileResponse {
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return nil
	}
	for _, d := range cerr.Details() {
		val, vErr := d.Value()
		if vErr != nil {
			continue
		}
		if snap, ok := val.(*v1.ReadFileResponse); ok {
			return snap
		}
	}
	return nil
}

// write_file

type WriteFileInput struct {
	Path            string `json:"path" jsonschema:"description=Path to the file. Relative to the workspace root; an absolute path inside the workspace also works."`
	Content         string `json:"content" jsonschema:"description=The full content to write to the file"`
	ExpectedVersion string `json:"expected_version,omitempty" jsonschema:"description=Version from the most recent read_file. Omit when creating a new file; required when overwriting."`
}

type WriteFileOutput struct {
	Version    string
	TotalLines int32
	Failure    string
	Snapshot   []TaggedLineView
}

type writeFileTool struct {
	toolkit *CodingToolkit
}

func (t *writeFileTool) Name() string { return "write_file" }
func (t *writeFileTool) Description() string {
	return "Create a new file (omit expected_version) or overwrite an existing one (provide expected_version from your most recent read_file). Unlike edit_file, expected_version is strict here — a mismatch rejects the write. Prefer edit_file for targeted changes."
}
func (t *writeFileTool) Schema() *WriteFileInput { return &WriteFileInput{} }

func (t *writeFileTool) Execute(ctx context.Context, input *WriteFileInput) (*WriteFileOutput, error) {
	resp, err := t.toolkit.runner.WriteFile(ctx, &v1.WriteFileRequest{
		WorkingDir:      t.toolkit.workingDir,
		Path:            input.Path,
		Content:         []byte(input.Content),
		ExpectedVersion: input.ExpectedVersion,
	})
	if err != nil {
		out := &WriteFileOutput{Failure: err.Error()}
		if snap := extractSnapshot(err); snap != nil {
			out.Version = snap.Version
			out.TotalLines = snap.TotalLines
			out.Snapshot = make([]TaggedLineView, len(snap.Lines))
			for i, l := range snap.Lines {
				out.Snapshot[i] = TaggedLineView{Line: l.Line, Hash: l.Hash, Content: l.Content}
			}
		}
		return out, nil
	}
	return &WriteFileOutput{Version: resp.Version, TotalLines: resp.TotalLines}, nil
}

func (t *writeFileTool) Render(o *WriteFileOutput) llms.ToolResult {
	if o.Failure != "" {
		var sb strings.Builder
		sb.WriteString("write_file failed: ")
		sb.WriteString(o.Failure)
		if len(o.Snapshot) > 0 {
			fmt.Fprintf(&sb, "\nfresh version: %s\ntotal_lines: %d\n", o.Version, o.TotalLines)
			for _, l := range o.Snapshot {
				fmt.Fprintf(&sb, "%d:%s|%s\n", l.Line, l.Hash, l.Content)
			}
		}
		return llms.TextToolResult(sb.String())
	}
	return llms.TextToolResult(fmt.Sprintf("Wrote file. Version: %s (%d lines)", o.Version, o.TotalLines))
}

// list_files

type ListFilesInput struct {
	Path     string `json:"path,omitempty" jsonschema:"description=Directory path relative to workspace root. Defaults to root if empty."`
	MaxDepth int    `json:"max_depth,omitempty" jsonschema:"description=Maximum depth to list files. Defaults to 1."`
}

type ListFilesOutput struct {
	Output string
}

type listFilesTool struct {
	toolkit *CodingToolkit
}

func (t *listFilesTool) Name() string { return "list_files" }
func (t *listFilesTool) Description() string {
	return "List files in the workspace. Use this to explore the project structure and understand the codebase layout."
}
func (t *listFilesTool) Schema() *ListFilesInput { return &ListFilesInput{} }

func (t *listFilesTool) Execute(ctx context.Context, input *ListFilesInput) (*ListFilesOutput, error) {
	dir := "."
	if input.Path != "" {
		dir = input.Path
	}

	args := []string{dir, "-type", "f"}
	if input.MaxDepth > 0 {
		args = append(args, "-maxdepth", strconv.Itoa(input.MaxDepth))
	} else {
		args = append(args, "-maxdepth", "1")
	}

	resp, err := t.toolkit.runner.Exec(ctx, &v1.ExecRequest{
		Command:        "find",
		Args:           args,
		WorkingDir:     t.toolkit.workingDir,
		MaxOutputBytes: uint32(limitForTool(t.Name()).bytes),
	})
	if err != nil {
		return nil, err
	}

	output := resp.Stdout
	if resp.Stderr != "" {
		output += "\n" + resp.Stderr
	}
	return &ListFilesOutput{Output: output}, nil
}

func (t *listFilesTool) Render(o *ListFilesOutput) llms.ToolResult {
	return llms.TextToolResult(o.Output)
}

// search_files

// The search runs ripgrep, which the VM image installs. Beyond being fast it
// is the tool whose defaults already match what a search of a working copy
// should mean: binary files skipped, .gitignore honored, so a pattern that
// happens to occur in a vendored bundle or a build artifact does not bury the
// occurrences in the source.
const (
	// defaultSearchResults is how many matches come back when the caller does
	// not ask for a number. Enough to see a pattern's shape, small enough that
	// a broad search is a cheap mistake rather than an expensive one.
	defaultSearchResults = 100
	// maxSearchResults is the ceiling on max_results. Past this the answer to
	// "there are too many matches" is a narrower search, not a longer list.
	maxSearchResults = 1000
	// searchLineColumns truncates long matching lines. A minified bundle or a
	// generated table can carry a match on a line thousands of columns wide,
	// and none of those columns are the reason the search was run.
	searchLineColumns = 200
	// searchTopFiles is how many of the heaviest files the overflow summary
	// names — enough to point at where the matches live.
	searchTopFiles = 3
	// searchCountsBytes bounds the per-file count query behind the overflow
	// summary. Its output is one short line per matching file, so this is
	// reached only by a search matching tens of thousands of files, which the
	// summary then reports as a floor rather than an exact total.
	searchCountsBytes = 64 * 1024
)

type SearchFilesInput struct {
	Pattern    string `json:"pattern" jsonschema:"description=Regex pattern to search for. Ripgrep syntax."`
	Path       string `json:"path,omitempty" jsonschema:"description=Directory or file path to search in relative to workspace root. Defaults to the whole workspace."`
	Glob       string `json:"glob,omitempty" jsonschema:"description=Glob filter (e.g. *.go or **/testdata/*). A glob without a slash matches by file name at any depth."`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"description=Maximum matches to return. Defaults to 100 and is capped at 1000."`
	FilesOnly  bool   `json:"files_only,omitempty" jsonschema:"description=Return just the paths of files containing a match instead of the matching lines. Use this to find where something lives."`
}

// FileMatchCount is one file's share of the matches, used to point at where an
// overflowing search's results are concentrated.
type FileMatchCount struct {
	Path    string
	Matches int
}

// SearchOverflow describes the matches a search could not return. It is
// gathered from a second, counting-only pass, so the model is told the size and
// the shape of what it is missing rather than being handed a silent prefix.
type SearchOverflow struct {
	TotalMatches int
	TotalFiles   int
	TopFiles     []FileMatchCount
	// Approximate is set when the counting pass hit its own output cap, making
	// the totals a floor rather than an exact figure.
	Approximate bool
}

type SearchFilesOutput struct {
	// Matches is one result per entry: "path:line:text", or a bare path when
	// FilesOnly.
	Matches []string
	// Overflow is set only when matches were left out.
	Overflow *SearchOverflow
	// Failure carries what the search itself complained about — an unreadable
	// path, an invalid pattern.
	Failure   string
	FilesOnly bool
}

type searchFilesTool struct {
	toolkit *CodingToolkit
}

func (t *searchFilesTool) Name() string { return "search_files" }
func (t *searchFilesTool) Description() string {
	return `Search for a regex pattern across the workspace. Returns matching lines as "path:line:text", newest-first by directory walk order.

Binary files are skipped and .gitignore is respected, so build output and vendored dependencies stay out of the results. Hidden files are searched; the .git directory is not.

Results are capped (100 by default, max_results up to 1000). When there are more matches than fit, the response says how many there are in total and which files hold most of them — narrow the search with path or glob rather than asking for a longer list. Set files_only to get just the paths of matching files, which is usually the faster way to find where something lives.`
}
func (t *searchFilesTool) Schema() *SearchFilesInput { return &SearchFilesInput{} }

func (t *searchFilesTool) Execute(ctx context.Context, input *SearchFilesInput) (*SearchFilesOutput, error) {
	limit := input.MaxResults
	if limit <= 0 {
		limit = defaultSearchResults
	}
	limit = min(limit, maxSearchResults)

	out := &SearchFilesOutput{FilesOnly: input.FilesOnly}

	// One line more than the limit is requested so overflow is known from the
	// search itself: exactly limit matches is a complete answer, one more than
	// that is a truncated one.
	resp, err := t.toolkit.runner.Exec(ctx, &v1.ExecRequest{
		Command:        t.searchCommand(input, limit+1),
		WorkingDir:     t.toolkit.workingDir,
		MaxOutputBytes: uint32(limitForTool(t.Name()).bytes),
	})
	if err != nil {
		return nil, err
	}

	lines := splitNonEmptyLines(resp.Stdout)
	if len(lines) == 0 && strings.TrimSpace(resp.Stderr) != "" {
		out.Failure = strings.TrimSpace(resp.Stderr)
		return out, nil
	}

	if len(lines) > limit {
		out.Matches = lines[:limit]
		out.Overflow = t.countMatches(ctx, input)
	} else {
		out.Matches = lines
	}
	return out, nil
}

// searchCommand builds the ripgrep pipeline for one search.
//
// The line limit is applied by head rather than in Go so that it bounds the
// work as well as the answer: ripgrep stops on the closed pipe instead of
// walking the rest of a large repository to produce matches nobody will read.
func (t *searchFilesTool) searchCommand(input *SearchFilesInput, lines int) string {
	args := []string{"rg"}
	args = append(args, t.filterArgs(input)...)
	if input.FilesOnly {
		args = append(args, "--files-with-matches")
	} else {
		args = append(args,
			"--line-number",
			"--no-heading",
			"--max-columns="+strconv.Itoa(searchLineColumns),
			"--max-columns-preview",
		)
	}
	args = append(args, t.patternArgs(input)...)

	return strings.Join(args, " ") + " | head -n " + strconv.Itoa(lines)
}

// filterArgs are the flags shared by the search and the counting pass, so the
// summary describes the same set of files the search looked at.
//
// Hidden files are searched because a workspace keeps real content in dotted
// directories — CI workflows, agent skills — but .git is excluded explicitly:
// ripgrep only leaves it alone while hidden files are off.
func (t *searchFilesTool) filterArgs(input *SearchFilesInput) []string {
	args := []string{"--color=never", "--hidden", "--glob=" + shellQuote("!.git/")}
	if input.Glob != "" {
		args = append(args, "--glob="+shellQuote(input.Glob))
	}
	return args
}

// patternArgs terminate the command line: -e keeps a pattern that begins with a
// dash from being read as a flag, and -- does the same for the path.
func (t *searchFilesTool) patternArgs(input *SearchFilesInput) []string {
	dir := "."
	if input.Path != "" {
		dir = input.Path
	}
	return []string{"-e", shellQuote(input.Pattern), "--", shellQuote(dir)}
}

// countMatches runs a second, counting-only pass to describe what the truncated
// search left behind. Its output is one line per matching file, which is a
// fraction of the size of the matches themselves.
//
// A failure here costs the summary, not the search: the matches already in hand
// are still worth returning.
func (t *searchFilesTool) countMatches(ctx context.Context, input *SearchFilesInput) *SearchOverflow {
	args := append([]string{"rg", "--count"}, t.filterArgs(input)...)
	args = append(args, t.patternArgs(input)...)

	resp, err := t.toolkit.runner.Exec(ctx, &v1.ExecRequest{
		Command:        strings.Join(args, " "),
		WorkingDir:     t.toolkit.workingDir,
		MaxOutputBytes: searchCountsBytes,
	})
	if err != nil {
		return nil
	}

	overflow := &SearchOverflow{Approximate: resp.StdoutTotalBytes > 0}
	for _, line := range splitNonEmptyLines(resp.Stdout) {
		// "path:count". A line that does not parse is the cap's marker or a
		// path containing a colon oddity; skipping it costs one file's share of
		// a total that is already labeled approximate.
		idx := strings.LastIndexByte(line, ':')
		if idx < 0 {
			continue
		}
		count, convErr := strconv.Atoi(line[idx+1:])
		if convErr != nil {
			continue
		}
		overflow.TotalFiles++
		overflow.TotalMatches += count
		overflow.TopFiles = append(overflow.TopFiles, FileMatchCount{Path: line[:idx], Matches: count})
	}

	if overflow.TotalFiles == 0 {
		return nil
	}

	slices.SortFunc(overflow.TopFiles, func(a, b FileMatchCount) int {
		if a.Matches != b.Matches {
			return b.Matches - a.Matches
		}
		return strings.Compare(a.Path, b.Path)
	})
	overflow.TopFiles = overflow.TopFiles[:min(searchTopFiles, len(overflow.TopFiles))]
	return overflow
}

func (t *searchFilesTool) Render(o *SearchFilesOutput) llms.ToolResult {
	if o.Failure != "" {
		return llms.TextToolResult("search_files failed: " + o.Failure)
	}
	if len(o.Matches) == 0 {
		return llms.TextToolResult("No matches.")
	}

	var sb strings.Builder
	for _, m := range o.Matches {
		sb.WriteString(m)
		sb.WriteString("\n")
	}

	if o.Overflow == nil {
		return llms.TextToolResult(sb.String())
	}

	about := ""
	if o.Overflow.Approximate {
		about = "at least "
	}
	if o.FilesOnly {
		fmt.Fprintf(&sb, "\n[kvarn: showing %d of %s%d files, holding %s%d matches.",
			len(o.Matches), about, o.Overflow.TotalFiles, about, o.Overflow.TotalMatches)
	} else {
		fmt.Fprintf(&sb, "\n[kvarn: showing %d of %s%d matches across %s%d files.",
			len(o.Matches), about, o.Overflow.TotalMatches, about, o.Overflow.TotalFiles)
	}
	if len(o.Overflow.TopFiles) > 0 {
		sb.WriteString(" Most matches:")
		for i, f := range o.Overflow.TopFiles {
			if i > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, " %s (%d)", f.Path, f.Matches)
		}
		sb.WriteString(".")
	}
	sb.WriteString(" Narrow the search with path or glob, or use a more specific pattern")
	if !o.FilesOnly {
		sb.WriteString("; files_only lists the matching files instead")
	}
	sb.WriteString(".]\n")

	return llms.TextToolResult(sb.String())
}

// splitNonEmptyLines splits command output into lines, dropping blank ones.
func splitNonEmptyLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// activate_skill

type ActivateSkillInput struct {
	Name string `json:"name" jsonschema:"description=Name of the skill to activate"`
}

type ActivateSkillOutput struct {
	Content string
}

type activateSkillTool struct {
	skills map[string]*repocontext.Skill
}

func (t *activateSkillTool) Name() string { return "activate_skill" }
func (t *activateSkillTool) Description() string {
	return "Load the full instructions for a skill. Use this when a task matches a skill's description from the available skills list."
}
func (t *activateSkillTool) Schema() *ActivateSkillInput { return &ActivateSkillInput{} }

func (t *activateSkillTool) Execute(_ context.Context, input *ActivateSkillInput) (*ActivateSkillOutput, error) {
	skill, ok := t.skills[input.Name]
	if !ok {
		return nil, fmt.Errorf("unknown skill %q", input.Name)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "<skill_content name=%q>\n", skill.Name)
	sb.WriteString(skill.Body)
	if !strings.HasSuffix(skill.Body, "\n") {
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, "\nSkill directory: %s\n", skill.Dir)
	sb.WriteString("Relative paths in this skill are relative to the skill directory.\n")

	if len(skill.Resources) > 0 {
		sb.WriteString("\n<skill_resources>\n")
		for _, r := range skill.Resources {
			fmt.Fprintf(&sb, "  <file>%s</file>\n", r)
		}
		sb.WriteString("</skill_resources>\n")
	}

	sb.WriteString("</skill_content>")

	return &ActivateSkillOutput{Content: sb.String()}, nil
}

func (t *activateSkillTool) Render(o *ActivateSkillOutput) llms.ToolResult {
	return llms.TextToolResult(o.Content)
}

// spawn_agent

type SpawnAgentInput struct {
	Name        string `json:"name" jsonschema:"description=Sub-agent name. Must match one of the registered sub-agents (see system prompt)."`
	Description string `json:"description" jsonschema:"description=Short description of why this sub-agent is being spawned. Shown in logs and the UI."`
	Prompt      string `json:"prompt" jsonschema:"description=The task the sub-agent should perform. Be specific and self-contained: the sub-agent does not see the parent conversation."`
}

type SpawnAgentOutput struct {
	Text string
}

type spawnAgentTool struct {
	toolkit *CodingToolkit
}

func (t *spawnAgentTool) Name() string { return "spawn_agent" }
func (t *spawnAgentTool) Description() string {
	return "Spawn a named sub-agent to perform a focused task. Sub-agents run in their own LLM loop with a restricted toolset and return a written answer. Multiple spawn_agent calls in the same turn run in parallel."
}
func (t *spawnAgentTool) Schema() *SpawnAgentInput { return &SpawnAgentInput{} }

func (t *spawnAgentTool) Execute(ctx context.Context, input *SpawnAgentInput) (*SpawnAgentOutput, error) {
	sub, ok := t.toolkit.subAgents[input.Name]
	if !ok {
		return nil, fmt.Errorf("unknown sub-agent %q", input.Name)
	}

	model, ok := t.toolkit.agentModels[sub.Name]
	if !ok {
		return nil, fmt.Errorf("sub-agent %q has no resolved model", sub.Name)
	}
	cfg := t.toolkit.agentConfigs[sub.Name]

	sessResp, err := t.toolkit.runner.CreateSession(ctx, &v1.CreateSessionRequest{
		WorkingDir: t.toolkit.workingDir,
	})
	if err != nil {
		return nil, fmt.Errorf("create sub-agent session: %w", err)
	}
	subSessionID := sessResp.SessionId
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = t.toolkit.runner.CloseSession(closeCtx, &v1.CloseSessionRequest{SessionId: subSessionID})
	}()

	runID, err := newRunID()
	if err != nil {
		return nil, fmt.Errorf("generate run id: %w", err)
	}
	subCtx := llms.WithStreamScope(ctx, llms.StreamScope{
		AgentID: sub.Name + "/" + runID,
		RunID:   runID,
	})

	deps := SubAgentDeps{
		Runner:     t.toolkit.runner,
		WorkingDir: t.toolkit.workingDir,
		SessionID:  subSessionID,
		Skills:     t.toolkit.skills,
	}

	maxOut := cfg.MaxOutputTokens
	if maxOut == 0 {
		maxOut = 16384
	}

	maxSteps := cfg.MaxSteps
	if maxSteps == 0 {
		maxSteps = 50
	}

	opts := []llms.GenerateOption{
		llms.WithSystemPrompt(sub.SystemPrompt(t.toolkit.repoCtx)),
		llms.WithMessages(llms.NewMessage(llms.RoleUser, llms.NewTextPart(input.Prompt))),
		llms.WithTools(sub.Tools(deps)...),
		llms.WithMaxSteps(maxSteps),
		llms.WithMaxOutputTokens(maxOut),
	}
	if cfg.ReasoningEffort != "" {
		opts = append(opts, llms.WithReasoningEffort(cfg.ReasoningEffort))
	}
	if parent := llms.GetExecutionContext(ctx); parent != nil {
		opts = append(opts, llms.WithParentExecution(parent))
	}

	result, err := model.GenerateContent(subCtx, opts...)
	if err != nil {
		return nil, fmt.Errorf("sub-agent %q: %w", sub.Name, err)
	}

	text := ""
	if tr, ok := result.(llms.TextResult); ok {
		text = tr.Text
	}
	return &SpawnAgentOutput{Text: text}, nil
}

func (t *spawnAgentTool) Render(o *SpawnAgentOutput) llms.ToolResult {
	return llms.TextToolResult(o.Text)
}

// newRunID returns a short random hex identifier for a sub-agent run.
func newRunID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
