package coding

import (
	"context"
	"fmt"
	"strings"
	"sync"

	llms "github.com/aholstenson/llms-go"

	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/sandbox"
)

// guestIdentity is who the merge commit made inside the VM is recorded as.
//
// It is deliberately not the identity the pull request will carry: this commit
// never leaves the guest. The host rebuilds the merge from the extracted
// worktree, under the project's configured commit author, and that is the commit
// the forge sees. This one exists so the guest's own `git log` reads sensibly.
var guestIdentity = sandbox.Identity{Name: "kvarn", Email: "kvarn@localhost"}

// mergeState is what the run remembers about its one merge. A run gets one:
// more than one would mean more than one merge commit, and nothing asks for
// that.
type mergeState struct {
	mu        sync.Mutex
	started   bool
	finished  bool
	conflicts []string
}

// merge_branch

type MergeBranchInput struct {
	Branch string `json:"branch,omitempty" jsonschema:"description=Branch to merge. Defaults to the pull request's base branch and no other branch can be named."`
}

type MergeBranchOutput struct {
	Branch          string
	AlreadyUpToDate bool
	Conflicts       []string
}

type mergeBranchTool struct {
	toolkit *CodingToolkit
}

func (t *mergeBranchTool) Name() string { return "merge_branch" }
func (t *mergeBranchTool) Description() string {
	return "Merge this pull request's base branch into the checkout. The merge is left staged and uncommitted so you can resolve any conflicts; call finish_merge when the tree is clean. Only the base branch can be merged, and only once per run — no other branch is present in this checkout."
}
func (t *mergeBranchTool) Schema() *MergeBranchInput { return &MergeBranchInput{} }

func (t *mergeBranchTool) Execute(ctx context.Context, input *MergeBranchInput) (*MergeBranchOutput, error) {
	target := t.toolkit.mergeTarget
	if target == nil {
		return nil, fmt.Errorf("this run has no branch to merge")
	}
	if b := strings.TrimSpace(input.Branch); b != "" && b != target.Branch {
		return nil, fmt.Errorf("only %q can be merged; %q is not in this checkout", target.Branch, b)
	}

	t.toolkit.merge.mu.Lock()
	defer t.toolkit.merge.mu.Unlock()
	if t.toolkit.merge.started {
		return nil, fmt.Errorf("a merge has already been made in this run; a run produces at most one merge commit")
	}

	status, err := sandbox.StartMerge(ctx, t.toolkit.runner, t.toolkit.workingDir, "refs/heads/"+target.Branch)
	if err != nil {
		return nil, err
	}
	if status.AlreadyUpToDate {
		// Nothing was started, so nothing has to be finished — and the run stays
		// free to merge later if the tree changes underneath it.
		return &MergeBranchOutput{Branch: target.Branch, AlreadyUpToDate: true}, nil
	}

	t.toolkit.merge.started = true
	t.toolkit.merge.conflicts = status.Conflicts
	return &MergeBranchOutput{Branch: target.Branch, Conflicts: status.Conflicts}, nil
}

func (t *mergeBranchTool) Render(o *MergeBranchOutput) llms.ToolResult {
	if o.AlreadyUpToDate {
		return llms.TextToolResult(fmt.Sprintf(
			"%s is already merged; there is nothing to merge and no merge to finish.", o.Branch))
	}
	if len(o.Conflicts) == 0 {
		return llms.TextToolResult(fmt.Sprintf(
			"Merged %s cleanly. The result is staged and uncommitted — call finish_merge to record it.", o.Branch))
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Merged %s with %d conflicting path(s):\n", o.Branch, len(o.Conflicts))
	for _, p := range o.Conflicts {
		fmt.Fprintf(&sb, "- %s\n", p)
	}
	sb.WriteString("\nResolve each one in the working tree, keeping both sides' intent, and remove every conflict marker. Then call finish_merge.")
	return llms.TextToolResult(sb.String())
}

// finish_merge

type FinishMergeInput struct {
	Notes string `json:"notes,omitempty" jsonschema:"description=One line on how the conflicts were resolved. Added to the merge commit message; leave it out when the merge was clean."`
}

type FinishMergeOutput struct {
	Message string
	Commit  string
}

type finishMergeTool struct {
	toolkit *CodingToolkit
}

func (t *finishMergeTool) Name() string { return "finish_merge" }
func (t *finishMergeTool) Description() string {
	return "Record the merge started by merge_branch as its own commit. It refuses while any path is still unmerged, while any changed file still holds a conflict marker, and if the merge moves a submodule. Anything you change after this lands in a second commit."
}
func (t *finishMergeTool) Schema() *FinishMergeInput { return &FinishMergeInput{} }

func (t *finishMergeTool) Execute(ctx context.Context, input *FinishMergeInput) (*FinishMergeOutput, error) {
	target := t.toolkit.mergeTarget
	if target == nil || t.toolkit.checkpointer == nil {
		return nil, fmt.Errorf("this run has no merge to finish")
	}

	t.toolkit.merge.mu.Lock()
	defer t.toolkit.merge.mu.Unlock()
	if !t.toolkit.merge.started {
		return nil, fmt.Errorf("no merge is in progress; call merge_branch first")
	}
	if t.toolkit.merge.finished {
		return nil, fmt.Errorf("the merge has already been finished")
	}

	// The message is built here rather than asked for, because the agent's
	// account of the run is not written until the summary call at the end — long
	// after this commit has to exist.
	message := mergeCommitMessage(target.Branch, t.toolkit.headBranch, t.toolkit.merge.conflicts, input.Notes)

	guestSHA, err := sandbox.FinishMerge(ctx, t.toolkit.runner, t.toolkit.workingDir, guestIdentity, message)
	if err != nil {
		return nil, err
	}

	if err := t.toolkit.checkpointer.Checkpoint(ctx, agent.CheckpointRequest{
		Kind:        agent.CheckpointMerge,
		Message:     message,
		MergeParent: target.SHA,
		GuestCommit: guestSHA,
	}); err != nil {
		return nil, fmt.Errorf("record the merge commit: %w", err)
	}

	t.toolkit.merge.finished = true
	return &FinishMergeOutput{Message: message, Commit: guestSHA}, nil
}

func (t *finishMergeTool) Render(o *FinishMergeOutput) llms.ToolResult {
	return llms.TextToolResult(fmt.Sprintf(
		"Merge committed as %s:\n\n%s\n\nAnything you change from here lands in a separate commit on top.",
		o.Commit, o.Message))
}

// mergeCommitMessage is the merge commit's subject and body: git's own wording
// for the subject, the resolved paths so a reader can see what needed a
// decision, and the agent's one-line account of how it decided.
func mergeCommitMessage(baseBranch, headBranch string, conflicts []string, notes string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Merge branch '%s' into '%s'", baseBranch, headBranch)

	if len(conflicts) > 0 {
		sb.WriteString("\n\nResolved conflicts:\n")
		for _, p := range conflicts {
			fmt.Fprintf(&sb, "- %s\n", p)
		}
	}
	if n := strings.TrimSpace(notes); n != "" {
		sb.WriteString("\n")
		// One line: a merge commit body is read next to the diff, and the account
		// of the whole run belongs in the pull request comment.
		sb.WriteString(firstLine(n))
		sb.WriteString("\n")
	}
	return sb.String()
}

// firstLine returns s up to its first newline.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
