package sandbox

import (
	"context"
	"strings"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/project"
	"github.com/cockroachdb/errors"
)

// dependencyInstallTimeoutSeconds caps a single `nix profile add` call.
// First-use Nix installs can pull large closures from cache.nixos.org.
const dependencyInstallTimeoutSeconds uint32 = 1800

// DependencyOutputFunc receives stdout/stderr from the install command.
type DependencyOutputFunc func(stdout, stderr string)

// InstallDependencies installs the given dependencies into the kvarn user's
// Nix profile in a single `nix profile add` invocation. One invocation
// (not per-attr) because Nix evaluates each flake once per call and merges
// all attrs into a single profile generation; per-attr would re-evaluate
// each flake N times.
func InstallDependencies(
	ctx context.Context,
	runner RunnerProxy,
	deps []project.ResolvedDep,
	onOutput DependencyOutputFunc,
) error {
	if len(deps) == 0 {
		return nil
	}

	var b strings.Builder
	b.WriteString("nix profile add --accept-flake-config")
	for _, d := range deps {
		b.WriteString(" ")
		b.WriteString(d.FlakeURI)
		b.WriteString("#")
		b.WriteString(d.Attr)
	}
	command := b.String()

	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command:        "su",
		Args:           []string{"-l", "-s", "/bin/sh", "-c", command, "--", "kvarn"},
		Privileged:     true,
		TimeoutSeconds: dependencyInstallTimeoutSeconds,
	})
	if err != nil {
		return errors.Wrap(err, "exec nix profile add")
	}
	if onOutput != nil {
		onOutput(resp.Stdout, resp.Stderr)
	}
	if resp.ExitCode != 0 {
		return errors.Newf("nix profile add failed (exit %d): %s", resp.ExitCode, resp.Stderr)
	}
	return nil
}
