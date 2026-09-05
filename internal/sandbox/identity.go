package sandbox

import (
	"context"
	"fmt"
	"strings"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
)

// Identity is the name and email a guest-side commit is recorded under.
type Identity struct {
	Name  string
	Email string
}

// DefaultIdentity is what the guest's git identity falls back to when the
// caller has no forge-configured commit author to hand over — a local run
// against a working directory, say. Any name works; having one is what
// matters.
var DefaultIdentity = Identity{Name: "kvarn", Email: "kvarn@localhost"}

// withDefaults fills in whichever half the caller left empty.
func (i Identity) withDefaults() Identity {
	if i.Name == "" {
		i.Name = DefaultIdentity.Name
	}
	if i.Email == "" {
		i.Email = DefaultIdentity.Email
	}
	return i
}

// ConfigureIdentity records id as the guest user's git identity.
//
// Nothing else configures git inside the VM, and git refuses to run without a
// committer identity — not only for `git commit` but for `git merge`, which
// checks for one before it starts even with `--no-commit`. Injecting the
// identity at boot is what keeps that from surfacing as a tool failure the
// agent then works around by hand, which is how a run ends up with commits
// nobody intended.
//
// It is written globally rather than into one repository so that it holds
// wherever git runs in the guest: the workspace, a nested repository a step
// creates, and every command the agent issues itself.
func ConfigureIdentity(ctx context.Context, runner RunnerProxy, id Identity) error {
	id = id.withDefaults()

	for _, setting := range [][2]string{{"user.name", id.Name}, {"user.email", id.Email}} {
		resp, err := runner.Exec(ctx, &v1.ExecRequest{
			Command: "git",
			// `--` so a name or email that starts with a dash is read as the
			// value it is rather than as an option.
			Args: []string{"config", "--global", "--", setting[0], setting[1]},
			// Unprivileged: the identity belongs to the user every command in
			// the workspace runs as, and root's config would not be read.
			Privileged: false,
		})
		if err != nil {
			return fmt.Errorf("set git %s: %w", setting[0], err)
		}
		if resp.ExitCode != 0 {
			return fmt.Errorf("set git %s failed (exit %d): %s",
				setting[0], resp.ExitCode, strings.TrimSpace(resp.Stderr))
		}
	}
	return nil
}
