package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/project"
	"github.com/bmatcuk/doublestar/v4"
	"github.com/cockroachdb/errors"
)

// StepResult captures the outcome of a single step execution.
type StepResult struct {
	Name     string
	ExitCode int32
	Stdout   string
	Stderr   string
	Skipped  bool  // true if skipped due to path filtering
	Err      error // non-nil if the RPC itself failed
}

// SetupResult captures the outcome of all setup steps and health checks.
type SetupResult struct {
	Steps        []StepResult
	HealthChecks []StepResult
}

// ValidationResult captures the outcome of all validation steps.
type ValidationResult struct {
	Required       []StepResult
	Advisory       []StepResult
	RequiredPassed bool
}

// OnStepDone is called after each step executes, for event emission.
type OnStepDone func(result StepResult, phase string)

// OnOutput is called with incremental stdout/stderr output from a running step.
type OnOutput func(stepName string, phase string, stdout string, stderr string)

// PullImage pulls a container image on the VM via the runner proxy.
func PullImage(ctx context.Context, runner RunnerProxy, image string) error {
	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command:    "podman",
		Args:       []string{"pull", image},
		WorkingDir: "/",
		Privileged: false,
	})
	if err != nil {
		return errors.Wrapf(err, "podman pull %s", image)
	}
	if resp.ExitCode != 0 {
		return errors.Newf("podman pull %s failed (exit %d): %s", image, resp.ExitCode, resp.Stderr)
	}
	return nil
}

// RunSetup executes setup steps followed by health checks.
// Setup short-circuits on the first failing step. Health checks run only if all setup steps pass.
func RunSetup(ctx context.Context, runner RunnerProxy, cfg *project.Config, sessionID string, onDone OnStepDone, onOutput OnOutput) (*SetupResult, error) {
	result := &SetupResult{}

	if cfg == nil {
		return result, nil
	}

	for _, step := range cfg.Setup.Steps {
		// Attempt the step up to 1+retry times. Each failed attempt is retried
		// silently; only the final result is recorded and reported.
		maxAttempts := 1 + int(step.Retry)
		var sr StepResult
		for attempt := 0; attempt < maxAttempts; attempt++ {
			sr = execStep(ctx, runner, step, sessionID, makeOutputCallback(onOutput, step.Name, "setup"))
			if sr.Err == nil && sr.ExitCode == 0 {
				break // success; no need to retry
			}
		}
		result.Steps = append(result.Steps, sr)
		if onDone != nil {
			onDone(sr, "setup")
		}
		if sr.Err != nil {
			return result, errors.Wrapf(sr.Err, "setup step %q", step.Name)
		}
		if sr.ExitCode != 0 {
			return result, errors.Newf("setup step %q failed with exit code %s", step.Name, formatExitCode(sr.ExitCode))
		}
	}

	for _, step := range cfg.Setup.HealthChecks {
		sr := execStep(ctx, runner, step, sessionID, makeOutputCallback(onOutput, step.Name, "health_check"))
		result.HealthChecks = append(result.HealthChecks, sr)
		if onDone != nil {
			onDone(sr, "health_check")
		}
		if sr.Err != nil {
			return result, errors.Wrapf(sr.Err, "health check %q", step.Name)
		}
		if sr.ExitCode != 0 {
			return result, errors.Newf("health check %q failed with exit code %s", step.Name, formatExitCode(sr.ExitCode))
		}
	}

	return result, nil
}

// RunValidation executes required and advisory validation steps.
// Required steps all run even if one fails. Advisory steps always run.
// Steps with paths filters are skipped if no changed files match.
func RunValidation(ctx context.Context, runner RunnerProxy, cfg *project.Config, sessionID string, changedFiles []string, onDone OnStepDone, onOutput OnOutput) (*ValidationResult, error) {
	result := &ValidationResult{
		RequiredPassed: true,
	}

	if cfg == nil {
		return result, nil
	}

	for _, step := range cfg.Validation.Required {
		if !shouldRun(step, changedFiles) {
			sr := StepResult{Name: step.Name, Skipped: true}
			result.Required = append(result.Required, sr)
			if onDone != nil {
				onDone(sr, "validation_required")
			}
			continue
		}

		sr := execStep(ctx, runner, step, sessionID, makeOutputCallback(onOutput, step.Name, "validation_required"))
		result.Required = append(result.Required, sr)
		if onDone != nil {
			onDone(sr, "validation_required")
		}
		if sr.Err != nil {
			result.RequiredPassed = false
		} else if sr.ExitCode != 0 {
			result.RequiredPassed = false
		}
	}

	for _, step := range cfg.Validation.Advisory {
		if !shouldRun(step, changedFiles) {
			sr := StepResult{Name: step.Name, Skipped: true}
			result.Advisory = append(result.Advisory, sr)
			if onDone != nil {
				onDone(sr, "validation_advisory")
			}
			continue
		}

		sr := execStep(ctx, runner, step, sessionID, makeOutputCallback(onOutput, step.Name, "validation_advisory"))
		result.Advisory = append(result.Advisory, sr)
		if onDone != nil {
			onDone(sr, "validation_advisory")
		}
	}

	return result, nil
}

// ChangedFiles runs `git diff --name-only HEAD` on the VM and returns the list of changed file paths.
func ChangedFiles(ctx context.Context, runner RunnerProxy, workspaceDir string) ([]string, error) {
	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command:    "git",
		Args:       []string{"diff", "--name-only", "HEAD"},
		WorkingDir: workspaceDir,
	})
	if err != nil {
		return nil, errors.Wrap(err, "git diff")
	}

	return parseFileList(resp.Stdout), nil
}

// shouldRun returns true if the step should execute given the changed files.
// If step.Paths is empty, always returns true. Otherwise returns true if any
// changed file matches any of the step's doublestar glob patterns.
func shouldRun(s project.Step, changedFiles []string) bool {
	if len(s.Paths) == 0 {
		return true
	}

	for _, cf := range changedFiles {
		for _, pattern := range s.Paths {
			matched, _ := doublestar.Match(pattern, cf)
			if matched {
				return true
			}
		}
	}

	return false
}

func execStep(ctx context.Context, runner RunnerProxy, step project.Step, sessionID string, onOutput OutputCallback) StepResult {
	cmd := step.Run
	if step.WorkingDir != "" {
		cmd = fmt.Sprintf("cd %s && %s", shellQuote(step.WorkingDir), step.Run)
	}

	resp, err := runner.SessionExec(ctx, &v1.SessionExecRequest{
		SessionId:      sessionID,
		Command:        cmd,
		TimeoutSeconds: step.Timeout.Seconds(),
	}, onOutput)
	if err != nil {
		return StepResult{
			Name: step.Name,
			Err:  err,
		}
	}

	return StepResult{
		Name:     step.Name,
		ExitCode: resp.ExitCode,
		Stdout:   resp.Stdout,
		Stderr:   resp.Stderr,
	}
}

// makeOutputCallback wraps an OnOutput callback into an OutputCallback
// with step name and phase baked in.
func makeOutputCallback(onOutput OnOutput, stepName string, phase string) OutputCallback {
	if onOutput == nil {
		return nil
	}
	return func(stdout, stderr string) {
		onOutput(stepName, phase, stdout, stderr)
	}
}

// formatExitCode returns a human-readable description of the exit code.
// Signal-killed processes (128+signal) include the signal name.
func formatExitCode(code int32) string {
	if code > 128 && code < 256 {
		sig := syscall.Signal(code - 128)
		name := sig.String()
		// Signal.String() returns "signal N" for unknown signals;
		// only include it if we got a real name.
		if !strings.HasPrefix(name, "signal ") {
			return fmt.Sprintf("%d (signal: %s)", code, name)
		}
	}
	return fmt.Sprintf("%d", code)
}

// shellQuote wraps s in single quotes, escaping any embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ExtractChanges copies changed files from the VM workspace back to destDir.
// It stages all changes, identifies modified/added/deleted files via git diff,
// reads each changed file from the VM, and writes/removes them in destDir.
func ExtractChanges(ctx context.Context, runner RunnerProxy, workspaceDir string, destDir string) error {
	// Stage all changes so we can diff against HEAD.
	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command:    "git",
		Args:       []string{"add", "-A"},
		WorkingDir: workspaceDir,
	})
	if err != nil {
		return errors.Wrap(err, "git add -A")
	}
	if resp.ExitCode != 0 {
		return errors.Newf("git add -A failed (exit %d): %s", resp.ExitCode, resp.Stderr)
	}

	// Get changed/added files.
	resp, err = runner.Exec(ctx, &v1.ExecRequest{
		Command:    "git",
		Args:       []string{"diff", "--cached", "--name-only", "--diff-filter=ACMR", "HEAD"},
		WorkingDir: workspaceDir,
	})
	if err != nil {
		return errors.Wrap(err, "git diff changed files")
	}
	changedFiles := parseFileList(resp.Stdout)

	// Get deleted files.
	resp, err = runner.Exec(ctx, &v1.ExecRequest{
		Command:    "git",
		Args:       []string{"diff", "--cached", "--name-only", "--diff-filter=D", "HEAD"},
		WorkingDir: workspaceDir,
	})
	if err != nil {
		return errors.Wrap(err, "git diff deleted files")
	}
	deletedFiles := parseFileList(resp.Stdout)

	slog.Info("extracting changes from VM",
		"changed", len(changedFiles),
		"deleted", len(deletedFiles),
	)

	// Stream each changed file from the VM back to destDir. StreamFromGuest
	// handles raw bytes (including non-UTF-8) and avoids the line-anchoring
	// machinery used by the agent-facing ReadFile.
	for _, f := range changedFiles {
		destPath := filepath.Join(destDir, f)
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return errors.Wrapf(err, "create dir for %s", f)
		}
		out, err := os.Create(destPath)
		if err != nil {
			return errors.Wrapf(err, "create file %s", f)
		}
		srcPath := filepath.Join(workspaceDir, f)
		if err := runner.StreamFromGuest(ctx, srcPath, out); err != nil {
			out.Close()
			return errors.Wrapf(err, "stream file %s from VM", f)
		}
		if err := out.Close(); err != nil {
			return errors.Wrapf(err, "close file %s", f)
		}
	}

	// Remove deleted files from destDir.
	for _, f := range deletedFiles {
		destPath := filepath.Join(destDir, f)
		if err := os.Remove(destPath); err != nil && !os.IsNotExist(err) {
			return errors.Wrapf(err, "remove deleted file %s", f)
		}
	}

	return nil
}

func parseFileList(output string) []string {
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files
}
