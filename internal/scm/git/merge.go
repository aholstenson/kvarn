package git

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/aholstenson/kvarn/internal/scm"
)

// MergeCommitOpts configures recording the worktree as a merge commit.
type MergeCommitOpts struct {
	RepoDir string
	Message string
	// MergeParent is the second parent: a commit already present in RepoDir.
	// It must be the base tip that was shipped into the VM rather than whatever
	// the base ref points at now, or the commit claims a parent the agent never
	// merged.
	MergeParent string
	AuthorName  string
	AuthorEmail string
}

// MergeCommit records the current worktree as a commit with two parents and
// moves HEAD onto it.
//
// It exists because a merge made inside the VM cannot travel back as a commit:
// changes come out of the guest as a flat diff, and committing that on the host
// would produce a single-parent commit claiming to be a merge. The forge would
// never see the pull request as merged with its base — a silent lie. So the
// merge is rebuilt here, from the guest's tree and the base commit the guest
// was given, which is the only pair that makes the parentage honest.
//
// Nothing is pushed. The commit stays in the clone until delivery, so a run
// that goes wrong after the merge leaves nothing behind on the forge.
func MergeCommit(ctx context.Context, opts MergeCommitOpts) (string, error) {
	if opts.RepoDir == "" {
		return "", errors.New("repo dir is required")
	}
	if opts.Message == "" {
		return "", errors.New("commit message is required")
	}
	if opts.MergeParent == "" {
		return "", errors.New("merge parent is required")
	}

	head, err := resolveCommit(ctx, opts.RepoDir, "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve HEAD: %w", err)
	}
	parent, err := resolveCommit(ctx, opts.RepoDir, opts.MergeParent)
	if err != nil {
		return "", fmt.Errorf("resolve merge parent %q: %w", opts.MergeParent, err)
	}

	if _, err := Run(ctx, Cmd{
		Dir:   opts.RepoDir,
		Sub:   "add",
		Flags: []string{"-A"},
	}); err != nil {
		return "", fmt.Errorf("stage merge result: %w", err)
	}

	tree, err := Run(ctx, Cmd{
		Dir: opts.RepoDir,
		Sub: "write-tree",
	})
	if err != nil {
		return "", fmt.Errorf("write tree: %w", err)
	}
	treeOID := strings.TrimSpace(tree)
	if !isObjectID(treeOID) {
		return "", fmt.Errorf("write-tree returned %q, want an object id", treeOID)
	}

	// commit-tree rather than commit, because commit would refuse to record a
	// second parent that the repository has no MERGE_HEAD for. The object ids
	// are git's own output or have been through rev-parse, so they are safe as
	// options; the message goes on stdin, as in CommitAndPush, so an agent-written
	// body never has to survive argv.
	out, err := Run(ctx, Cmd{
		Dir: opts.RepoDir,
		Config: []string{
			"user.name=" + opts.AuthorName,
			"user.email=" + opts.AuthorEmail,
			"commit.gpgsign=false",
		},
		Sub:   "commit-tree",
		Flags: []string{treeOID, "-p", head, "-p", parent, "-F", "-"},
		Stdin: opts.Message,
	})
	if err != nil {
		return "", fmt.Errorf("create merge commit: %w", err)
	}
	sha := strings.TrimSpace(out)
	if !isObjectID(sha) {
		return "", fmt.Errorf("commit-tree returned %q, want an object id", sha)
	}

	// A ref write rather than `git reset`, for the same reason CommitAndPush
	// uses one: the index and worktree already hold the merge result and must
	// not be touched.
	if _, err := Run(ctx, Cmd{
		Dir:      opts.RepoDir,
		Sub:      "update-ref",
		Operands: []string{"HEAD", sha},
	}); err != nil {
		return "", fmt.Errorf("move HEAD onto the merge commit: %w", err)
	}

	slog.Info("recorded merge commit", "sha", sha, "merge_parent", parent)
	return sha, nil
}

// FetchBranchOpts configures bringing one branch into an existing repository.
type FetchBranchOpts struct {
	RepoDir string
	// Source is where the objects come from: a mirror path on the host, or the
	// forge URL when there is no mirror. The clone has no remote of its own —
	// see SanitizeClone — so it is always named explicitly.
	Source      string
	Branch      string
	Credentials scm.CredentialSource
}

// FetchBranch brings Branch into RepoDir as refs/heads/<branch>.
//
// It is how the base branch of a pull request reaches a job clone that was made
// `--single-branch`: the branch is simply not there otherwise, and a merge has
// nothing to merge from. The clone's whole .git travels into the VM, so a ref
// fetched here arrives in the guest with it.
func FetchBranch(ctx context.Context, opts FetchBranchOpts) error {
	if opts.RepoDir == "" {
		return errors.New("repo dir is required")
	}
	if opts.Source == "" {
		return errors.New("source is required")
	}
	if opts.Branch == "" {
		return errors.New("branch is required")
	}

	auth, err := ResolveAuth(ctx, opts.Source, opts.Credentials)
	if err != nil {
		return err
	}
	defer auth.Close()

	refspec := "+refs/heads/" + opts.Branch + ":refs/heads/" + opts.Branch
	if _, err := Run(ctx, Cmd{
		Dir:      opts.RepoDir,
		Config:   auth.Config,
		Env:      auth.Env,
		Sub:      "fetch",
		Flags:    []string{"--no-tags"},
		Operands: []string{opts.Source, refspec},
	}); err != nil {
		return fmt.Errorf("fetch branch %s: %w", opts.Branch, err)
	}
	return nil
}

// UnshallowOpts configures completing a shallow repository's history.
type UnshallowOpts struct {
	RepoDir     string
	Source      string
	Credentials scm.CredentialSource
	// Refs are the refs whose history to complete, spelled as they exist in the
	// source (e.g. "refs/heads/main"). They are fetched without a destination
	// so that objects arrive without git having to write the branch the
	// repository currently has checked out, which it refuses to do.
	Refs []string
}

// Unshallow completes the history of a shallow clone from source.
//
// It is the escape hatch for a merge base that lies deeper than the clone's
// depth: without the commit where the two branches parted, git cannot compute
// what the merge should do, and a merge it computes anyway is wrong in a way
// nobody would notice until the diff was read. The cost is disk rather than
// network when the source is the host's own mirror.
//
// A repository that is not shallow is left alone: git rejects --unshallow there,
// and there is nothing to complete.
func Unshallow(ctx context.Context, opts UnshallowOpts) error {
	if opts.RepoDir == "" {
		return errors.New("repo dir is required")
	}
	if opts.Source == "" {
		return errors.New("source is required")
	}
	if len(opts.Refs) == 0 {
		return errors.New("at least one ref is required")
	}
	shallow, err := IsShallow(ctx, opts.RepoDir)
	if err != nil {
		return err
	}
	if !shallow {
		return nil
	}

	auth, err := ResolveAuth(ctx, opts.Source, opts.Credentials)
	if err != nil {
		return err
	}
	defer auth.Close()

	if _, err := Run(ctx, Cmd{
		Dir:      opts.RepoDir,
		Config:   auth.Config,
		Env:      auth.Env,
		Sub:      "fetch",
		Flags:    []string{"--unshallow", "--no-tags"},
		Operands: append([]string{opts.Source}, opts.Refs...),
	}); err != nil {
		return fmt.Errorf("unshallow: %w", err)
	}
	return nil
}

// IsShallow reports whether repoDir has a truncated history.
func IsShallow(ctx context.Context, repoDir string) (bool, error) {
	out, err := Run(ctx, Cmd{
		Dir:   repoDir,
		Sub:   "rev-parse",
		Flags: []string{"--is-shallow-repository"},
	})
	if err != nil {
		return false, fmt.Errorf("check shallowness: %w", err)
	}
	return strings.TrimSpace(out) == "true", nil
}

// HasMergeBase reports whether the two revisions share an ancestor the
// repository actually holds. A shallow clone can answer no to a pair that does
// share one upstream, which is exactly the case Unshallow exists for.
func HasMergeBase(ctx context.Context, repoDir, a, b string) (bool, error) {
	_, err := Run(ctx, Cmd{
		Dir:      repoDir,
		Sub:      "merge-base",
		Operands: []string{a, b},
	})
	if err == nil {
		return true, nil
	}
	// merge-base exits 1 when there is no common ancestor and 128 when it could
	// not read a ref. Only the second is a fault worth reporting; the first is
	// the answer.
	if isExitCode(err, 1) {
		return false, nil
	}
	return false, fmt.Errorf("merge-base %s %s: %w", a, b, err)
}

// DeleteBranch removes a local branch ref, used to undo a base branch that
// turned out not to be usable rather than shipping a repository whose refs
// promise more than they hold.
func DeleteBranch(ctx context.Context, repoDir, branch string) error {
	if _, err := Run(ctx, Cmd{
		Dir:      repoDir,
		Sub:      "update-ref",
		Flags:    []string{"-d"},
		Operands: []string{"refs/heads/" + branch},
	}); err != nil {
		return fmt.Errorf("delete branch %s: %w", branch, err)
	}
	return nil
}

// ResolveRef returns the object id of the commit a revision names.
func ResolveRef(ctx context.Context, repoDir, rev string) (string, error) {
	return resolveCommit(ctx, repoDir, rev)
}

// resolveCommit turns a revision into the object id of the commit it names,
// which is both a validation and what lets the id be passed as an option
// afterwards.
func resolveCommit(ctx context.Context, repoDir, rev string) (string, error) {
	out, err := Run(ctx, Cmd{
		Dir:      repoDir,
		Sub:      "rev-parse",
		Flags:    []string{"--verify", "--quiet"},
		Operands: []string{rev + "^{commit}"},
	})
	if err != nil {
		return "", err
	}
	oid := strings.TrimSpace(out)
	if !isObjectID(oid) {
		return "", fmt.Errorf("%q does not name a commit", rev)
	}
	return oid, nil
}

// objectIDRe matches a full object id in either hash format git supports.
var objectIDRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func isObjectID(s string) bool { return objectIDRe.MatchString(s) }

// isExitCode reports whether err came from a command that exited with code.
func isExitCode(err error, code int) bool {
	var ee interface{ ExitCode() int }
	return errors.As(err, &ee) && ee.ExitCode() == code
}
