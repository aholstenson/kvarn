package sandbox

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/project"
)

const (
	// dependencyInstallAttemptTimeout caps a single `nix profile add` call.
	// First-use Nix installs can pull large closures from cache.nixos.org.
	dependencyInstallAttemptTimeout = 30 * time.Minute

	// dependencyInstallBudget bounds every attempt together. Without it a
	// retried install could hold a VM for a multiple of the per-attempt cap;
	// with it, this is the longest the caller ever waits.
	dependencyInstallBudget = 45 * time.Minute

	// dependencyInstallMinAttempt is the least remaining budget worth another
	// attempt. An install started with less than this in hand would be killed
	// mid-download and report a timeout instead of the network failure that
	// actually caused the retries.
	dependencyInstallMinAttempt = 2 * time.Minute

	// dependencyFailureDetailRunes caps the failure text carried in the error.
	dependencyFailureDetailRunes = 2000
)

// dependencyRetryBackoff is the pause before each retry, so its length is also
// the number of retries. Nix retries downloads itself first, five times a few
// hundred milliseconds apart, which only covers a blip; these waits are sized
// for a forge or binary cache that is unwell for a minute or two.
var dependencyRetryBackoff = []time.Duration{15 * time.Second, 45 * time.Second}

// nixTransientErrorSignatures are the failures worth another attempt: the far
// end was reachable but did not serve the request. A missing attribute or an
// unknown flake fails identically every time, so a failure that matches none
// of these is reported at once instead of after the full backoff.
var nixTransientErrorSignatures = []string{
	// A forge or binary cache answering with an error page. GitHub's API
	// serves a 504 for a branch lookup under load, and rate limiting shows up
	// as 429 once several jobs start at the same moment.
	"HTTP error 5",
	"HTTP error 429",
	// Name resolution and connection setup.
	"Couldn't resolve host",
	"Could not resolve host",
	"Temporary failure in name resolution",
	"Connection timed out",
	"Connection reset by peer",
	"Connection refused",
	"SSL connect error",
	// Transfers that started and did not finish.
	"Timeout was reached",
	"Operation too slow",
	"transferred a partial file",
	"unexpected end-of-file",
}

// DependencyOutputFunc receives stdout/stderr from the install command.
type DependencyOutputFunc func(stdout, stderr string)

// InstallDependencies installs the given dependencies into the kvarn user's
// Nix profile in a single `nix profile add` invocation. One invocation
// (not per-attr) because Nix evaluates each flake once per call and merges
// all attrs into a single profile generation; per-attr would re-evaluate
// each flake N times.
//
// A transient network failure is retried. Repeating the whole command is safe:
// Nix evaluates and builds every installable before it switches the profile
// generation, and that switch is a single atomic operation, so a failed
// attempt leaves the profile exactly as it was.
func InstallDependencies(
	ctx context.Context,
	runner RunnerProxy,
	deps []project.ResolvedDep,
	onOutput DependencyOutputFunc,
) error {
	if len(deps) == 0 {
		return nil
	}

	command := nixProfileAddCommand(deps)
	maxAttempts := len(dependencyRetryBackoff) + 1
	deadline := time.Now().Add(dependencyInstallBudget)

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		remaining := time.Until(deadline)
		if remaining < dependencyInstallMinAttempt {
			break
		}

		resp, err := runner.Exec(ctx, &v1.ExecRequest{
			Command:        "su",
			Args:           []string{"-l", "-s", "/bin/sh", "-c", command, "--", "kvarn"},
			Privileged:     true,
			TimeoutSeconds: uint32(min(dependencyInstallAttemptTimeout, remaining).Seconds()),
		})
		if err != nil {
			// Failing to run the command at all says the bridge or the VM is
			// in doubt, and no amount of retrying inside that VM repairs it.
			return fmt.Errorf("exec nix profile add: %w", err)
		}
		if onOutput != nil {
			onOutput(resp.Stdout, resp.Stderr)
		}
		if resp.ExitCode == 0 {
			return nil
		}

		lastErr = fmt.Errorf("nix profile add failed (exit %d) after %s: %s",
			resp.ExitCode, attemptCount(attempt), nixFailureDetail(resp.Stderr))
		if !isTransientNixFailure(resp.Stderr) || attempt == maxAttempts {
			break
		}

		wait := dependencyRetryBackoff[attempt-1]
		// The notice rides the install's own output stream: a viewer reads the
		// install as one phase, and a silent wait looks like a hang.
		if onOutput != nil {
			onOutput("", fmt.Sprintf(
				"kvarn: dependency install hit a network failure; retrying in %s (attempt %d of %d)\n",
				wait, attempt+1, maxAttempts))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", ctx.Err(), lastErr)
		case <-time.After(wait):
		}
	}

	if lastErr == nil {
		// Only reachable if the budget was already spent before the first
		// attempt, which means the caller handed over an exhausted deadline.
		return fmt.Errorf("nix profile add: no time left in the %s dependency install budget",
			dependencyInstallBudget)
	}
	return lastErr
}

// pinNixpkgsRevs rewrites each nixpkgs channel name in a dependency's flake
// reference to the commit that channel points at, so the install starts from a
// tarball rather than from a branch lookup the guest has to make first.
//
// Only FlakeURI changes. Channel keeps naming the channel, which is what cache
// keys are derived from, so a channel that has merely moved does not invalidate
// a project's caches. One lookup per distinct channel serves every attribute
// installed from it.
func pinNixpkgsRevs(ctx context.Context, deps []project.ResolvedDep, revOf NixpkgsRevFunc) []project.ResolvedDep {
	if revOf == nil {
		return deps
	}
	out := make([]project.ResolvedDep, len(deps))
	copy(out, deps)
	resolved := map[string]string{}
	for i, d := range out {
		if d.Channel == "" {
			continue
		}
		rev, seen := resolved[d.Channel]
		if !seen {
			rev = revOf(ctx, d.Channel)
			resolved[d.Channel] = rev
		}
		if rev != "" {
			out[i].FlakeURI = project.NixpkgsFlakePrefix + rev
		}
	}
	return out
}

// nixProfileAddCommand builds the shell command that installs every resolved
// dependency in one go.
func nixProfileAddCommand(deps []project.ResolvedDep) string {
	var b strings.Builder
	b.WriteString("nix profile add --accept-flake-config")
	for _, d := range deps {
		b.WriteString(" ")
		b.WriteString(d.FlakeURI)
		b.WriteString("#")
		b.WriteString(d.Attr)
	}
	return b.String()
}

// isTransientNixFailure reports whether a failed install is worth retrying.
// Only the error Nix ended on is considered: a download that failed and then
// succeeded on Nix's own retry still leaves its `warning:` lines in the
// output, so matching the whole stream would retry evaluation errors that
// merely happen to follow a recovered download.
func isTransientNixFailure(stderr string) bool {
	block := nixErrorBlock(stderr)
	if block == "" {
		return false
	}
	for _, sig := range nixTransientErrorSignatures {
		if strings.Contains(block, sig) {
			return true
		}
	}
	return false
}

// nixErrorBlock returns stderr from its first `error:` line on, which is where
// Nix says what it gave up on. Everything above is progress and warnings.
// `error (ignored):` is deliberately not matched — Nix carried on past it.
func nixErrorBlock(stderr string) string {
	lines := strings.Split(stderr, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "error:") {
			return strings.Join(lines[i:], "\n")
		}
	}
	return ""
}

// nixFailureDetail is the failure text an error carries: the error Nix ended
// on rather than the whole transcript. An install prints screens of download
// progress and per-attempt warnings before it fails, and repeating all of it
// buries the one line that says why.
func nixFailureDetail(stderr string) string {
	detail := nixErrorBlock(stderr)
	if detail == "" {
		detail = stderr
	}
	detail = strings.TrimSpace(detail)
	if r := []rune(detail); len(r) > dependencyFailureDetailRunes {
		detail = string(r[:dependencyFailureDetailRunes]) + "… (output truncated)"
	}
	return detail
}

// attemptCount renders an attempt tally for an error message.
func attemptCount(n int) string {
	if n == 1 {
		return "1 attempt"
	}
	return fmt.Sprintf("%d attempts", n)
}
