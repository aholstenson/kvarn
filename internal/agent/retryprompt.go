package agent

import (
	"fmt"
	"strings"

	"github.com/aholstenson/kvarn/internal/sandbox"
)

// MaxRetryPromptStepBytes caps how much of a single failing step's stderr we
// echo back to the agent. The agent already has the workspace and can re-run
// the step itself; the orchestrator/CLI only needs to point at what broke.
const MaxRetryPromptStepBytes = 4 * 1024

// BuildRetryPrompt renders a user message describing the failing validation
// steps. It is appended to the agent's existing conversation between attempts
// so the agent sees its prior turn plus the new failure signal.
//
// Steps that passed are intentionally omitted; including them would just add
// noise. RPC-layer errors (val.Required[i].Err != nil) are surfaced too so
// the agent doesn't mistake a runner failure for a code defect.
//
// attempt is 1-indexed for display (i.e. the retry number, not the loop
// counter). maxAttempts is the configured retry cap, so the prompt shows
// "attempt N of M+1" — including the original try in the denominator.
func BuildRetryPrompt(val *sandbox.ValidationResult, attempt, maxAttempts int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Required validation steps failed on attempt %d of %d. ",
		attempt, maxAttempts+1)
	sb.WriteString("Your previous change is still in the workspace — iterate on it to fix the failures below rather than reverting. ")
	sb.WriteString("After making changes, the validation steps will run again automatically.\n\n")

	if val == nil {
		sb.WriteString("(no validation result available)")
		return sb.String()
	}

	any := false
	for _, step := range val.Required {
		if step.Skipped {
			continue
		}
		if step.Err == nil && step.ExitCode == 0 {
			continue
		}
		any = true
		sb.WriteString("- Step ")
		sb.WriteString(step.Name)
		if step.Err != nil {
			sb.WriteString(" failed to run: ")
			sb.WriteString(step.Err.Error())
			sb.WriteString("\n")
			continue
		}
		fmt.Fprintf(&sb, " exited %d\n", step.ExitCode)

		out := step.Stderr
		label := "stderr"
		if strings.TrimSpace(out) == "" {
			out = step.Stdout
			label = "stdout"
		}
		out = strings.TrimSpace(out)
		if out == "" {
			continue
		}
		if len(out) > MaxRetryPromptStepBytes {
			out = "…(truncated)…\n" + out[len(out)-MaxRetryPromptStepBytes:]
		}
		sb.WriteString("  ")
		sb.WriteString(label)
		sb.WriteString(":\n")
		for _, line := range strings.Split(out, "\n") {
			sb.WriteString("  | ")
			sb.WriteString(line)
			sb.WriteString("\n")
		}
	}

	if !any {
		sb.WriteString("(no failing steps reported; this likely means a transient runner error)")
	}
	return sb.String()
}
