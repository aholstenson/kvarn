package sandbox

import (
	"context"
	"fmt"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
)

// CheckoutWorktree materializes the worktree of an already-transferred
// repository inside the guest.
//
// It exists so a pristine clone can cross the transport once as packed objects
// instead of twice — once as objects and once as the expanded files git would
// write from them anyway. It must run before anything else reads or writes the
// workspace, since until it returns the directory holds only ".git".
//
// The LFS overrides are load-bearing rather than defensive. Running the real
// smudge filter would both change what lands on disk (real blobs instead of the
// pointer files a job sees today) and make the guest authenticate to an LFS
// endpoint — a credential path the sandbox is built to make impossible. Forcing
// `cat` keeps pointer files verbatim, and clearing `required` stops a repository
// whose .gitattributes marks the filter mandatory from failing the checkout.
func CheckoutWorktree(ctx context.Context, runner RunnerProxy, workingDir string) error {
	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command: "git",
		Args: []string{
			"-c", "filter.lfs.smudge=cat",
			"-c", "filter.lfs.required=false",
			"reset", "--hard", "HEAD",
		},
		WorkingDir: workingDir,
		// Unprivileged, so the files land owned by kvarn like every other
		// path into the workspace.
		Privileged: false,
	})
	if err != nil {
		return fmt.Errorf("git reset --hard: %w", err)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("git reset --hard failed (exit %d): %s", resp.ExitCode, resp.Stderr)
	}
	return nil
}
