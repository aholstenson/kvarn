package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/aholstenson/kvarn/internal/agent"
	gitscm "github.com/aholstenson/kvarn/internal/scm/git"
)

// runCheckpointer turns a boundary the agent reaches mid-run into a commit in
// the job clone.
//
// It is the host half of the merge: the guest resolves the merge and says so,
// and this extracts the tree at that moment and records it with the two parents
// the merge actually had. Nothing is pushed — the clone stays local until
// delivery, so the property that nothing leaves the host mid-run is preserved,
// and a run that fails after a merge leaves the pull request untouched.
type runCheckpointer struct {
	sess        Sandbox
	cloneDir    string
	authorName  string
	authorEmail string
	log         *slog.Logger

	mu    sync.Mutex
	count int
}

// Checkpoint satisfies agent.Checkpointer.
func (c *runCheckpointer) Checkpoint(ctx context.Context, req agent.CheckpointRequest) error {
	if req.Kind != agent.CheckpointMerge {
		return fmt.Errorf("unsupported checkpoint kind %d", req.Kind)
	}
	if req.MergeParent == "" {
		return fmt.Errorf("a merge checkpoint needs the commit that was merged")
	}
	if req.GuestCommit == "" {
		return fmt.Errorf("a merge checkpoint needs the commit the guest recorded")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.sess.ExtractChanges(ctx, c.cloneDir); err != nil {
		return fmt.Errorf("extract the merge result from the VM: %w", err)
	}

	sha, err := gitscm.MergeCommit(ctx, gitscm.MergeCommitOpts{
		RepoDir:     c.cloneDir,
		Message:     req.Message,
		MergeParent: req.MergeParent,
		AuthorName:  c.authorName,
		AuthorEmail: c.authorEmail,
	})
	if err != nil {
		return fmt.Errorf("record the merge commit: %w", err)
	}

	// Every later extraction now measures against the commit the guest made, so
	// the trailing commit carries what the agent did after the merge and not the
	// merge all over again.
	c.sess.SetBaseCommit(req.GuestCommit)

	c.count++
	c.log.Info("recorded a merge commit",
		"sha", sha, "merge_parent", req.MergeParent, "guest_commit", req.GuestCommit)
	return nil
}

// Count is how many commits the run has already recorded. Delivery reads it:
// with no changed files left but a commit already made, there is still
// something to push.
//
// Nil-safe, because most runs have no checkpointer at all and every caller
// would otherwise have to say so.
func (c *runCheckpointer) Count() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}
