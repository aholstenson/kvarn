package sandbox

import (
	"context"
	"fmt"
	"strings"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
)

// MergeStatus is what a started merge left behind.
type MergeStatus struct {
	// AlreadyUpToDate reports that the ref was already an ancestor of HEAD, so
	// no merge was started and there is nothing to finish.
	AlreadyUpToDate bool
	// Conflicts are the repository-relative paths git could not merge on its
	// own. Empty means the merge applied cleanly and is staged, waiting to be
	// finished.
	Conflicts []string
}

// conflictMarker is the opening line of a conflict git writes into a file it
// could not merge. A file still carrying one has been left half-resolved, and
// committing it would put the marker in the pull request.
const conflictMarker = "^<<<<<<< "

// StartMerge merges ref into the guest's checkout without committing.
//
// `--no-commit` on both the clean and the conflicted path is deliberate: the
// merge always ends at FinishMerge, so the commit boundary is in one place
// whether or not git had to ask the agent for help. The LFS overrides match
// CheckoutWorktree's — the guest must not run a smudge filter that would
// authenticate to an LFS endpoint.
func StartMerge(ctx context.Context, runner RunnerProxy, workspaceDir string, ref string) (*MergeStatus, error) {
	if ref == "" {
		return nil, fmt.Errorf("merge ref is required")
	}

	// Asked before the merge rather than read out of git's English afterwards:
	// "Already up to date." is a message, and this is a fact.
	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command:    "git",
		Args:       []string{"merge-base", "--is-ancestor", ref, "HEAD"},
		WorkingDir: workspaceDir,
	})
	if err != nil {
		return nil, fmt.Errorf("check whether %s is already merged: %w", ref, err)
	}
	if resp.ExitCode == 0 {
		return &MergeStatus{AlreadyUpToDate: true}, nil
	}
	if resp.ExitCode > 1 {
		// 1 is the answer "no"; anything above it is git failing to read a ref.
		return nil, fmt.Errorf("check whether %s is already merged failed (exit %d): %s",
			ref, resp.ExitCode, strings.TrimSpace(resp.Stderr))
	}

	resp, err = runner.Exec(ctx, &v1.ExecRequest{
		Command: "git",
		Args: []string{
			"-c", "filter.lfs.smudge=cat",
			"-c", "filter.lfs.required=false",
			"merge", "--no-ff", "--no-commit", ref,
		},
		WorkingDir: workspaceDir,
	})
	if err != nil {
		return nil, fmt.Errorf("git merge %s: %w", ref, err)
	}
	if resp.ExitCode == 0 {
		return &MergeStatus{}, nil
	}

	conflicts, err := unmergedPaths(ctx, runner, workspaceDir)
	if err != nil {
		return nil, err
	}
	if len(conflicts) == 0 {
		// A non-zero exit with nothing unmerged is git refusing to start at all
		// — a dirty worktree, an unknown ref — and reporting it as a conflicted
		// merge would send the agent looking for conflicts that do not exist.
		return nil, fmt.Errorf("git merge %s failed (exit %d): %s", ref, resp.ExitCode, strings.TrimSpace(resp.Stderr))
	}
	return &MergeStatus{Conflicts: conflicts}, nil
}

// FinishMerge commits the merge the guest has in progress and returns its
// commit id.
//
// The checks in front of the commit are what keep a half-resolved tree from
// reaching a pull request. They run here, in the guest, because this is the
// moment the tree is live on disk: extraction reads files from the worktree,
// so a merge has to be recorded when it is resolved rather than replayed from
// guest history at the end of the run.
func FinishMerge(ctx context.Context, runner RunnerProxy, workspaceDir string, id Identity, message string) (string, error) {
	if message == "" {
		return "", fmt.Errorf("merge commit message is required")
	}
	if id.Name == "" || id.Email == "" {
		return "", fmt.Errorf("commit identity is required")
	}

	inProgress, err := MergeInProgress(ctx, runner, workspaceDir)
	if err != nil {
		return "", err
	}
	if !inProgress {
		return "", fmt.Errorf("no merge is in progress")
	}

	// Staging comes first because the agent resolves a conflict by editing the
	// file, not by running git: until the result is staged, git still calls the
	// path unmerged however well it has been resolved. What decides whether the
	// tree is resolved is therefore its content, which is what the marker search
	// below reads.
	if _, err := runner.Exec(ctx, &v1.ExecRequest{
		Command:    "git",
		Args:       []string{"add", "-A"},
		WorkingDir: workspaceDir,
	}); err != nil {
		return "", fmt.Errorf("stage merge result: %w", err)
	}

	// An invariant rather than a policy: staging resolves every conflict, so
	// anything still unmerged here is git telling us something we do not
	// understand, and committing on top of it would bury that.
	unmerged, err := unmergedPaths(ctx, runner, workspaceDir)
	if err != nil {
		return "", err
	}
	if len(unmerged) > 0 {
		return "", fmt.Errorf("%d path(s) are still unmerged after staging: %s",
			len(unmerged), strings.Join(unmerged, ", "))
	}

	// Everything the merge staged is compared against HEAD rather than against
	// the run's base: HEAD is the pull request's head commit, so this is exactly
	// what the merge is bringing in.
	changed, submodules, err := mergedEntries(ctx, runner, workspaceDir)
	if err != nil {
		return "", err
	}
	if len(submodules) > 0 {
		// Extraction streams files out of the worktree and skips submodule
		// entries, so a merge that moves a submodule pointer would reach the
		// pull request with that part of it missing. Failing loudly is the only
		// honest outcome.
		return "", fmt.Errorf("the merge changes submodule(s) %s, which kvarn cannot carry out of the VM; merge this pull request by hand",
			strings.Join(submodules, ", "))
	}

	marked, err := pathsWithConflictMarkers(ctx, runner, workspaceDir, changed)
	if err != nil {
		return "", err
	}
	if len(marked) > 0 {
		return "", fmt.Errorf("%d file(s) still contain conflict markers: %s",
			len(marked), strings.Join(marked, ", "))
	}

	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command: "git",
		Args: []string{
			"-c", "user.name=" + id.Name,
			"-c", "user.email=" + id.Email,
			"-c", "commit.gpgsign=false",
			"commit", "--no-verify", "-m", message,
		},
		WorkingDir: workspaceDir,
	})
	if err != nil {
		return "", fmt.Errorf("commit merge: %w", err)
	}
	if resp.ExitCode != 0 {
		return "", fmt.Errorf("commit merge failed (exit %d): %s", resp.ExitCode, strings.TrimSpace(resp.Stderr))
	}

	sha := ResolveBaseCommit(ctx, runner, workspaceDir)
	if sha == "" {
		return "", fmt.Errorf("could not read the merge commit back")
	}
	return sha, nil
}

// MergeInProgress reports whether the guest has a merge waiting to be
// committed. It is the end-of-run guard: a run that leaves one behind has a
// half-resolved tree, which must never be delivered.
func MergeInProgress(ctx context.Context, runner RunnerProxy, workspaceDir string) (bool, error) {
	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command:    "git",
		Args:       []string{"rev-parse", "--verify", "--quiet", "MERGE_HEAD"},
		WorkingDir: workspaceDir,
	})
	if err != nil {
		return false, fmt.Errorf("check for a merge in progress: %w", err)
	}
	return resp.ExitCode == 0, nil
}

// unmergedPaths lists the paths git left with an unresolved conflict.
func unmergedPaths(ctx context.Context, runner RunnerProxy, workspaceDir string) ([]string, error) {
	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command:    "git",
		Args:       []string{"diff", "--name-only", "--diff-filter=U"},
		WorkingDir: workspaceDir,
	})
	if err != nil {
		return nil, fmt.Errorf("list unmerged paths: %w", err)
	}
	if resp.ExitCode != 0 {
		return nil, fmt.Errorf("list unmerged paths failed (exit %d): %s", resp.ExitCode, strings.TrimSpace(resp.Stderr))
	}
	return parseFileList(resp.Stdout), nil
}

// mergedEntries reports what the staged merge changes relative to HEAD: the
// paths, and separately any submodule among them.
func mergedEntries(ctx context.Context, runner RunnerProxy, workspaceDir string) (changed []string, submodules []string, err error) {
	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command:    "git",
		Args:       []string{"diff", "--cached", "--raw", "-z", "HEAD"},
		WorkingDir: workspaceDir,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("read the merge's diff: %w", err)
	}
	if resp.ExitCode != 0 {
		return nil, nil, fmt.Errorf("read the merge's diff failed (exit %d): %s", resp.ExitCode, strings.TrimSpace(resp.Stderr))
	}

	// Deleted paths are dropped: they have no post-image mode to inspect and no
	// content left to search for markers.
	entries, _, err := parseRawDiff(resp.Stdout)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if e.mode == gitModeSubmodule {
			submodules = append(submodules, e.path)
			continue
		}
		changed = append(changed, e.path)
	}
	return changed, submodules, nil
}

// grepBatch bounds how many paths one search names. A merge can touch thousands
// of files, and a single command line holding all of them is one the guest's
// exec would refuse.
const grepBatch = 100

// pathsWithConflictMarkers returns those of paths whose content still carries a
// conflict marker. The search is scoped to what the merge changed: a marker in
// a test fixture elsewhere in the repository is not this merge's problem.
func pathsWithConflictMarkers(ctx context.Context, runner RunnerProxy, workspaceDir string, paths []string) ([]string, error) {
	var marked []string
	for start := 0; start < len(paths); start += grepBatch {
		batch := paths[start:min(start+grepBatch, len(paths))]

		// -I skips binary files, whose bytes can hold anything. git grep exits 1
		// when nothing matched, which is an answer rather than a failure.
		args := append([]string{"grep", "--no-color", "-I", "-l", "-e", conflictMarker, "--"}, batch...)
		resp, err := runner.Exec(ctx, &v1.ExecRequest{
			Command:    "git",
			Args:       args,
			WorkingDir: workspaceDir,
		})
		if err != nil {
			return nil, fmt.Errorf("search for conflict markers: %w", err)
		}
		if resp.ExitCode > 1 {
			return nil, fmt.Errorf("search for conflict markers failed (exit %d): %s",
				resp.ExitCode, strings.TrimSpace(resp.Stderr))
		}
		marked = append(marked, parseFileList(resp.Stdout)...)
	}
	return marked, nil
}
