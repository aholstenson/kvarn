package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
)

// A worktree snapshot is a git tree object that holds every file in the
// workspace that is tracked or not ignored. It is built in a temporary index,
// so the workspace's own index, and anything staged in it, stays as it was.
//
// The temporary index starts as a copy of the workspace's own. That keeps
// tracked files that a .gitignore pattern matches in the snapshot, because
// changes to them are part of the delivered change too. It also lets git reuse
// the file stat data the index already has, so only the files that changed are
// read and hashed again.
//
// Changes leave the VM as a diff of the live worktree, so anything that writes
// to the workspace writes into the delivered change. A snapshot lets a step
// that must not change the workspace — an agent that checks the change — still
// run commands that write files, because the workspace is put back after it.
const (
	// worktreeIndexSetup prepares the temporary index and stages the whole
	// workspace into it. Both scripts start with it.
	worktreeIndexSetup = `set -e
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
index=$(git rev-parse --git-path index)
if [ -f "$index" ]; then cp "$index" "$dir/index"; fi
export GIT_INDEX_FILE="$dir/index"
git add -A .
`

	snapshotScript = worktreeIndexSetup + `git write-tree`

	// restoreScript moves the workspace from what it holds now to the snapshot
	// tree in $1. A two-tree read-tree changes only the files that differ and
	// removes the files the snapshot does not have, so the files the check did
	// not touch keep their contents and timestamps. --reset lets it replace a
	// file that has become ignored since the snapshot. Ignored files that the
	// snapshot does not have stay: they are not part of the delivered change
	// either. It then stages the workspace again and checks that it now holds
	// exactly the snapshot, so a restore that did not take is an error and not
	// a quiet success. It prints "restored" when it had to change something.
	restoreScript = worktreeIndexSetup + `current=$(git write-tree)
if [ "$current" = "$1" ]; then exit 0; fi
git read-tree --reset -u "$current" "$1"
git add -A .
if [ "$(git write-tree)" != "$1" ]; then
  echo "the workspace still differs from the snapshot after the restore" >&2
  exit 1
fi
echo restored`
)

// SnapshotWorktree records the files in the workspace at dir and returns the
// tree that holds them.
func SnapshotWorktree(ctx context.Context, runner RunnerProxy, dir string) (string, error) {
	out, err := runWorktreeScript(ctx, runner, dir, snapshotScript)
	if err != nil {
		return "", fmt.Errorf("snapshot workspace: %w", err)
	}
	tree := strings.TrimSpace(out)
	if tree == "" {
		return "", errors.New("snapshot workspace: git write-tree printed no tree")
	}
	return tree, nil
}

// RestoreWorktree puts the workspace at dir back to the snapshot tree if it no
// longer matches it. It reports whether it had to put anything back.
//
// A failed restore is tried once more. The usual cause is a file that was
// still being written when the first try ran, and the second try starts from
// whatever the workspace holds by then.
func RestoreWorktree(ctx context.Context, runner RunnerProxy, dir, tree string) (bool, error) {
	var err error
	for range 2 {
		var out string
		out, err = runWorktreeScript(ctx, runner, dir, restoreScript, tree)
		if err == nil {
			return strings.TrimSpace(out) == "restored", nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return false, fmt.Errorf("restore workspace: %w", err)
}

func runWorktreeScript(ctx context.Context, runner RunnerProxy, dir, script string, args ...string) (string, error) {
	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command:    "sh",
		Args:       append([]string{"-c", script, "sh"}, args...),
		WorkingDir: dir,
	})
	if err != nil {
		return "", err
	}
	if resp.ExitCode != 0 {
		return "", fmt.Errorf("exit code %d: %s", resp.ExitCode, strings.TrimSpace(resp.Stderr))
	}
	return resp.Stdout, nil
}
