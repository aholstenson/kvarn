package coding

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
	runner     sandbox.RunnerProxy
	workingDir string
	sessionID  string
	skills     map[string]*repocontext.Skill
	models     map[string]llms.Model
	configs    map[string]modelcfg.Entry
	subAgents  SubAgents
	repoCtx    *repocontext.RepoContext
	tracker    *cost.Tracker
	tasks      *TaskList
}

// CodingToolkitOpts configures a CodingToolkit. Models, SubAgents, and RepoCtx
// are required when the parent agent should be able to spawn sub-agents; the
// spawn_agent tool is omitted from the toolkit when SubAgents is empty or no
// models are available.
type CodingToolkitOpts struct {
	Runner     sandbox.RunnerProxy
	WorkingDir string
	SessionID  string
	Skills     []repocontext.Skill
	// Models maps alias (e.g. ModelMain, ModelSmall) to a resolved LLM.
	Models map[string]llms.Model
	// Configs carries the resolved per-alias settings (thinking budget, max
	// output tokens). May be nil if sub-agents are not used.
	Configs   map[string]modelcfg.Entry
	SubAgents SubAgents
	RepoCtx   *repocontext.RepoContext
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
		runner:     opts.Runner,
		workingDir: opts.WorkingDir,
		sessionID:  opts.SessionID,
		skills:     skillMap,
		models:     opts.Models,
		configs:    opts.Configs,
		subAgents:  opts.SubAgents,
		repoCtx:    opts.RepoCtx,
		tracker:    opts.Tracker,
		tasks:      NewTaskList(),
	}
}

// withBudgetWarning wraps each ToolDef so that, after the warn threshold is
// crossed, the next tool result the model sees gains a wrap-up note from the
// tracker. When no tracker is set, the input slice is returned unchanged.
func (t *CodingToolkit) withBudgetWarning(tools []llms.ToolDef) []llms.ToolDef {
	if t.tracker == nil {
		return tools
	}
	wrapped := make([]llms.ToolDef, len(tools))
	for i, td := range tools {
		wrapped[i] = &budgetWrappedTool{inner: td, tracker: t.tracker}
	}
	return wrapped
}

// budgetWrappedTool decorates a ToolDef so that ToString optionally appends a
// one-shot budget warning. All other behavior is forwarded verbatim.
type budgetWrappedTool struct {
	inner   llms.ToolDef
	tracker *cost.Tracker
}

func (w *budgetWrappedTool) Name() string                                  { return w.inner.Name() }
func (w *budgetWrappedTool) Description() string                           { return w.inner.Description() }
func (w *budgetWrappedTool) Schema() any                                   { return w.inner.Schema() }
func (w *budgetWrappedTool) Execute(ctx context.Context, in any) (any, error) {
	return w.inner.Execute(ctx, in)
}

func (w *budgetWrappedTool) ToString(out any) string {
	s := w.inner.ToString(out)
	if note, ok := w.tracker.ConsumeWarning(); ok {
		if s != "" {
			s += "\n\n"
		}
		s += note
	}
	return s
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
	if len(t.models) > 0 && len(t.subAgents) > 0 {
		tools = append(tools, llms.NewToolDef(&spawnAgentTool{toolkit: t}))
	}
	return t.withBudgetWarning(tools)
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
	if len(t.models) > 0 && len(t.subAgents) > 0 {
		tools = append(tools, llms.NewToolDef(&spawnAgentTool{toolkit: t}))
	}
	return t.withBudgetWarning(tools)
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

	resp, err := t.toolkit.runner.SessionExec(ctx, &v1.SessionExecRequest{
		SessionId: t.toolkit.sessionID,
		Command:   cmd,
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

func (t *execCommandTool) ToString(o *ExecCommandOutput) string {
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
	return sb.String()
}

// read_file

type ReadFileInput struct {
	Path      string `json:"path" jsonschema:"description=Path to the file relative to the workspace root"`
	StartLine int    `json:"start_line,omitempty" jsonschema:"description=1-indexed start line. Omit or set to 0 for whole file."`
	EndLine   int    `json:"end_line,omitempty" jsonschema:"description=1-indexed inclusive end line. Omit or set to 0 for whole file."`
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

Optional start_line / end_line (1-indexed, inclusive) limit the response to a window. The version always covers the whole file.`
}
func (t *readFileTool) Schema() *ReadFileInput { return &ReadFileInput{} }

func (t *readFileTool) Execute(ctx context.Context, input *ReadFileInput) (*ReadFileOutput, error) {
	resp, err := t.toolkit.runner.ReadFile(ctx, &v1.ReadFileRequest{
		WorkingDir: t.toolkit.workingDir,
		Path:       input.Path,
		StartLine:  int32(input.StartLine),
		EndLine:    int32(input.EndLine),
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

func (t *readFileTool) ToString(o *ReadFileOutput) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "version: %s\n", o.Version)
	fmt.Fprintf(&sb, "total_lines: %d\n", o.TotalLines)
	for _, l := range o.Lines {
		fmt.Fprintf(&sb, "%d:%s|%s\n", l.Line, l.Hash, l.Content)
	}
	return sb.String()
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
	Path            string               `json:"path" jsonschema:"description=Path to the file relative to the workspace root"`
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

func (t *editFileTool) ToString(o *EditFileOutput) string {
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
		return sb.String()
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Edit applied. New version: %s (%d lines total)\n", o.Version, o.TotalLines)
	if o.VersionDrift {
		sb.WriteString("Note: the file changed elsewhere since your last read. Distant anchors may be stale.\n")
	}
	for _, l := range o.Context {
		fmt.Fprintf(&sb, "%d:%s|%s\n", l.Line, l.Hash, l.Content)
	}
	return sb.String()
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
	Path            string `json:"path" jsonschema:"description=Path to the file relative to the workspace root"`
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

func (t *writeFileTool) ToString(o *WriteFileOutput) string {
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
		return sb.String()
	}
	return fmt.Sprintf("Wrote file. Version: %s (%d lines)", o.Version, o.TotalLines)
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
		Command:    "find",
		Args:       args,
		WorkingDir: t.toolkit.workingDir,
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

func (t *listFilesTool) ToString(o *ListFilesOutput) string {
	return o.Output
}

// search_files

type SearchFilesInput struct {
	Pattern string `json:"pattern" jsonschema:"description=Regex pattern to search for"`
	Path    string `json:"path,omitempty" jsonschema:"description=Directory path to search in relative to workspace root. Defaults to root if empty."`
	Glob    string `json:"glob,omitempty" jsonschema:"description=File glob filter (e.g. *.go or *.py)"`
}

type SearchFilesOutput struct {
	Output string
}

type searchFilesTool struct {
	toolkit *CodingToolkit
}

func (t *searchFilesTool) Name() string { return "search_files" }
func (t *searchFilesTool) Description() string {
	return "Search for a regex pattern across files. Returns matching lines with file paths and line numbers. Supports optional file glob filter."
}
func (t *searchFilesTool) Schema() *SearchFilesInput { return &SearchFilesInput{} }

func (t *searchFilesTool) Execute(ctx context.Context, input *SearchFilesInput) (*SearchFilesOutput, error) {
	args := []string{"-rn"}

	if input.Glob != "" {
		args = append(args, "--include="+input.Glob)
	}

	args = append(args, input.Pattern)

	dir := "."
	if input.Path != "" {
		dir = input.Path
	}
	args = append(args, dir)

	resp, err := t.toolkit.runner.Exec(ctx, &v1.ExecRequest{
		Command:    "grep",
		Args:       args,
		WorkingDir: t.toolkit.workingDir,
	})
	if err != nil {
		return nil, err
	}

	output := resp.Stdout
	if resp.Stderr != "" {
		output += "\n" + resp.Stderr
	}
	return &SearchFilesOutput{Output: output}, nil
}

func (t *searchFilesTool) ToString(o *SearchFilesOutput) string {
	return o.Output
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

func (t *activateSkillTool) ToString(o *ActivateSkillOutput) string {
	return o.Content
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

	alias := sub.Model
	if alias == "" {
		alias = ModelMain
	}
	model, ok := t.toolkit.models[alias]
	if !ok {
		return nil, fmt.Errorf("sub-agent %q requires model %q which is not configured", sub.Name, alias)
	}

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

	maxOut := sub.MaxOutputTokens
	if maxOut == 0 {
		maxOut = 16384
	}

	maxSteps := sub.MaxSteps
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
	if sub.ThinkingTokens > 0 {
		opts = append(opts, llms.WithMaxThinkingTokens(sub.ThinkingTokens))
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

func (t *spawnAgentTool) ToString(o *SpawnAgentOutput) string {
	return o.Text
}

// newRunID returns a short random hex identifier for a sub-agent run.
func newRunID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
