package sandbox

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
)

// These helpers find and stop the processes a step leaves behind. Closing a
// shell session kills its process group, but a process can leave that group —
// `setsid`, a daemonizing server, a rootless container's monitor — and go on
// writing to the workspace after the step is over. The guest runs one job, as
// one user, so a process of that user that did not exist before the step was
// started by it.
//
// They act on every process of the user the runner runs commands as. In the
// guest that is the job's own user; anywhere else it would be the caller's
// whole login, so they belong to the guest only.

// listUserProcessesScript prints the IDs of the user's processes, one per
// line, leaving out the shell that runs it.
const listUserProcessesScript = `ps -u "$(id -u)" -o pid= | while read -r pid; do
  [ "$pid" = "$$" ] || echo "$pid"
done`

// killNewProcessesScript kills every process of the user whose ID is not one
// of its arguments, leaving out the shell that runs it, and prints how many it
// killed. A process that ends between the listing and the kill is not an error.
const killNewProcessesScript = `keep=" $* "
n=0
for pid in $(ps -u "$(id -u)" -o pid=); do
  [ "$pid" = "$$" ] && continue
  case "$keep" in *" $pid "*) continue ;; esac
  if kill -KILL "$pid" 2>/dev/null; then n=$((n+1)); fi
done
echo "$n"`

// UserProcesses lists the IDs of the processes the runner's user has now. A
// caller keeps the list to stop what a step starts later; see
// KillProcessesExcept.
func UserProcesses(ctx context.Context, runner RunnerProxy) ([]string, error) {
	out, err := runProcessScript(ctx, runner, listUserProcessesScript)
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	return strings.Fields(out), nil
}

// KillProcessesExcept kills every process of the runner's user that is not in
// keep, and returns how many it killed.
func KillProcessesExcept(ctx context.Context, runner RunnerProxy, keep []string) (int, error) {
	out, err := runProcessScript(ctx, runner, killNewProcessesScript, keep...)
	if err != nil {
		return 0, fmt.Errorf("stop leftover processes: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("stop leftover processes: unexpected output %q", strings.TrimSpace(out))
	}
	return n, nil
}

func runProcessScript(ctx context.Context, runner RunnerProxy, script string, args ...string) (string, error) {
	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command: "sh",
		Args:    append([]string{"-c", script, "sh"}, args...),
	})
	if err != nil {
		return "", err
	}
	if resp.ExitCode != 0 {
		return "", fmt.Errorf("exit code %d: %s", resp.ExitCode, strings.TrimSpace(resp.Stderr))
	}
	return resp.Stdout, nil
}
