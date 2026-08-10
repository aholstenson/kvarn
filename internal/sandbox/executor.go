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
		return fmt.Errorf("podman pull %s: %w", image, err)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("podman pull %s failed (exit %d): %s", image, resp.ExitCode, resp.Stderr)
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
			return result, fmt.Errorf("setup step %q: %w", step.Name, sr.Err)
		}
		if sr.ExitCode != 0 {
			return result, fmt.Errorf("setup step %q failed with exit code %s", step.Name, formatExitCode(sr.ExitCode))
		}
	}

	for _, step := range cfg.Setup.HealthChecks {
		sr := execStep(ctx, runner, step, sessionID, makeOutputCallback(onOutput, step.Name, "health_check"))
		result.HealthChecks = append(result.HealthChecks, sr)
		if onDone != nil {
			onDone(sr, "health_check")
		}
		if sr.Err != nil {
			return result, fmt.Errorf("health check %q: %w", step.Name, sr.Err)
		}
		if sr.ExitCode != 0 {
			return result, fmt.Errorf("health check %q failed with exit code %s", step.Name, formatExitCode(sr.ExitCode))
		}
	}

	return result, nil
}

// RunValidation executes required and advisory validation steps.
// Required steps all run even if one fails. Advisory steps always run.
//
// changedFiles gates the steps that declare paths. A nil list means the caller
// has no diff to gate on — a read-only run that was never going to write one,
// or `kvarn test` running the suite outright — and every step runs. An empty
// non-nil list is a real "nothing changed", which skips them.
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

// ChangedFiles runs `git diff --name-only <base>` on the VM and returns the
// list of changed file paths. The list is never nil: a run that changed nothing
// has answered the question, which is not the same as not being asked, and only
// the second means "run every step" to shouldRun.
//
// See BaseRef for why the comparison is against a recorded base rather than
// HEAD.
func ChangedFiles(ctx context.Context, runner RunnerProxy, workspaceDir string, baseRef string) ([]string, error) {
	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command:    "git",
		Args:       []string{"diff", "--name-only", BaseRef(baseRef)},
		WorkingDir: workspaceDir,
	})
	if err != nil {
		return nil, fmt.Errorf("git diff: %w", err)
	}

	return parseFileList(resp.Stdout), nil
}

// BaseRef resolves the revision a workspace's changes are measured against.
// The base is the commit the workspace was created at, not HEAD: agents are
// free to commit inside the VM, and against HEAD everything they committed
// would read as "no changes" and never leave the sandbox. An empty base means
// the commit could not be resolved (no repository, or no commits yet), and
// HEAD is then the only ref there is.
func BaseRef(baseRef string) string {
	if baseRef == "" {
		return "HEAD"
	}
	return baseRef
}

// ResolveBaseCommit reads the commit a freshly prepared workspace sits at, to
// be passed back as the baseRef of later change detection. It returns "" when
// the workspace is not a git repository or has no commits yet, which callers
// treat as "compare against HEAD".
func ResolveBaseCommit(ctx context.Context, runner RunnerProxy, workspaceDir string) string {
	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command:    "git",
		Args:       []string{"rev-parse", "HEAD"},
		WorkingDir: workspaceDir,
	})
	if err != nil || resp.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(resp.Stdout)
}

// shouldRun returns true if the step should execute given the changed files.
// A step that declares no paths always runs, and so does every step when there
// is no diff to gate on: a nil changedFiles is "the caller cannot say", and
// silently skipping the path-scoped steps there would report a green result for
// a suite that never ran. Otherwise the step runs when any changed file matches
// one of its doublestar patterns.
func shouldRun(s project.Step, changedFiles []string) bool {
	if len(s.Paths) == 0 || changedFiles == nil {
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

// The complete set of modes git can record for a blob in a tree. Git tracks
// only the executable bit for regular files, so nothing else can appear.
const (
	gitModeRegular    = "100644"
	gitModeExecutable = "100755"
	gitModeSymlink    = "120000"
	gitModeSubmodule  = "160000"
)

// changedEntry is the post-image side of one `git diff --raw` record: where
// the entry lives, what git says it is, and the blob holding its content.
type changedEntry struct {
	path string // repository-relative path
	mode string // one of the gitMode* constants
	blob string // post-image blob hash
}

// ExtractChanges copies changed files from the VM workspace back to destDir.
// It stages all changes, diffs against baseRef (see BaseRef), then recreates
// each entry in destDir with the mode git recorded for it and removes the paths
// that went away. destDir is expected to hold the pre-image (a clone at the same
// base commit), so the result is a worktree that git sees as exactly the VM's
// diff.
func ExtractChanges(ctx context.Context, runner RunnerProxy, workspaceDir string, destDir string, baseRef string) error {
	// Stage all changes so we can diff against the base commit.
	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command:    "git",
		Args:       []string{"add", "-A"},
		WorkingDir: workspaceDir,
	})
	if err != nil {
		return fmt.Errorf("git add -A: %w", err)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("git add -A failed (exit %d): %s", resp.ExitCode, resp.Stderr)
	}

	// --raw carries the post-image mode and blob hash next to each path. That
	// is what lets us restore the executable bit and recreate symlinks as
	// links; a name-only listing would flatten everything to a 0644 file.
	resp, err = runner.Exec(ctx, &v1.ExecRequest{
		Command:    "git",
		Args:       []string{"diff", "--cached", "--raw", "-z", BaseRef(baseRef)},
		WorkingDir: workspaceDir,
	})
	if err != nil {
		return fmt.Errorf("git diff --raw: %w", err)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("git diff --raw failed (exit %d): %s", resp.ExitCode, resp.Stderr)
	}

	changed, deleted, err := parseRawDiff(resp.Stdout)
	if err != nil {
		return err
	}

	slog.Info("extracting changes from VM",
		"changed", len(changed),
		"deleted", len(deleted),
	)

	// Deletions run first: a path can swap kind between the two revisions (a
	// file replaced by a directory, a symlink replaced by a real directory),
	// and clearing the old entry keeps the writes below from colliding with it.
	for _, f := range deleted {
		destPath, err := resolveDest(destDir, f)
		if err != nil {
			return err
		}
		if err := os.Remove(destPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove deleted file %s: %w", f, err)
		}
	}

	for _, c := range changed {
		if err := extractEntry(ctx, runner, workspaceDir, destDir, c); err != nil {
			return err
		}
	}

	return nil
}

// extractEntry materializes one changed entry in destDir with the kind and
// mode git recorded for it in the VM.
func extractEntry(ctx context.Context, runner RunnerProxy, workspaceDir string, destDir string, c changedEntry) error {
	if c.mode == gitModeSubmodule {
		// A submodule is a commit pointer, not content we can stream out.
		slog.Warn("skipping submodule while extracting changes", "path", c.path)
		return nil
	}

	destPath, err := resolveDest(destDir, c.path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("create dir for %s: %w", c.path, err)
	}
	// Replace rather than overwrite: the entry may have changed kind, and
	// writing into an existing symlink would follow it out of destDir.
	if err := os.Remove(destPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("replace %s: %w", c.path, err)
	}

	if c.mode == gitModeSymlink {
		target, err := readBlob(ctx, runner, workspaceDir, c.blob)
		if err != nil {
			return fmt.Errorf("read symlink target for %s: %w", c.path, err)
		}
		if err := os.Symlink(target, destPath); err != nil {
			return fmt.Errorf("create symlink %s: %w", c.path, err)
		}
		return nil
	}

	var perm os.FileMode
	switch c.mode {
	case gitModeRegular:
		perm = 0o644
	case gitModeExecutable:
		perm = 0o755
	default:
		return fmt.Errorf("unsupported git mode %s for %s", c.mode, c.path)
	}

	// Stream the file from the VM. StreamFromGuest handles raw bytes
	// (including non-UTF-8) and avoids the line-anchoring machinery used by
	// the agent-facing ReadFile.
	out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("create file %s: %w", c.path, err)
	}
	srcPath := filepath.Join(workspaceDir, c.path)
	if err := runner.StreamFromGuest(ctx, srcPath, out); err != nil {
		out.Close()
		return fmt.Errorf("stream file %s from VM: %w", c.path, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close file %s: %w", c.path, err)
	}
	// The create above is subject to the process umask, so set the mode git
	// recorded explicitly.
	if err := os.Chmod(destPath, perm); err != nil {
		return fmt.Errorf("chmod %s: %w", c.path, err)
	}

	return nil
}

// readBlob reads a blob out of the VM repository by hash. A symlink is stored
// as a blob holding its target, and streaming the path itself would follow the
// link and yield the target's content instead.
func readBlob(ctx context.Context, runner RunnerProxy, workspaceDir string, hash string) (string, error) {
	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command:    "git",
		Args:       []string{"cat-file", "blob", hash},
		WorkingDir: workspaceDir,
	})
	if err != nil {
		return "", err
	}
	if resp.ExitCode != 0 {
		return "", fmt.Errorf("git cat-file %s failed (exit %d): %s", hash, resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout == "" {
		return "", fmt.Errorf("blob %s is empty", hash)
	}
	return resp.Stdout, nil
}

// parseRawDiff parses `git diff --raw -z` output into the entries to write and
// the paths to remove. Each record is
//
//	:<srcmode> <dstmode> <srcsha> <dstsha> <status>\0<path>\0
//
// with a second path for renames and copies. The -z form is what makes this
// safe for paths containing spaces, quotes or newlines, which git would
// otherwise escape.
func parseRawDiff(output string) (changed []changedEntry, deleted []string, err error) {
	fields := strings.Split(output, "\x00")
	for i := 0; i < len(fields); {
		meta := fields[i]
		if meta == "" {
			// Trailing NUL terminator of the last record.
			i++
			continue
		}
		if !strings.HasPrefix(meta, ":") {
			return nil, nil, fmt.Errorf("unexpected git diff --raw record %q", meta)
		}
		parts := strings.Fields(strings.TrimPrefix(meta, ":"))
		if len(parts) != 5 {
			return nil, nil, fmt.Errorf("malformed git diff --raw record %q", meta)
		}
		dstMode, dstBlob, status := parts[1], parts[3], parts[4]

		// Renames and copies name both the source and the destination.
		numPaths := 1
		if status[0] == 'R' || status[0] == 'C' {
			numPaths = 2
		}
		if i+numPaths >= len(fields) {
			return nil, nil, fmt.Errorf("truncated git diff --raw record %q", meta)
		}
		paths := fields[i+1 : i+1+numPaths]
		i += 1 + numPaths

		switch status[0] {
		case 'D':
			deleted = append(deleted, paths[0])
		case 'R':
			// A rename is a write at the new path plus a removal at the old
			// one; nothing else reports the old path as gone.
			deleted = append(deleted, paths[0])
			changed = append(changed, changedEntry{path: paths[1], mode: dstMode, blob: dstBlob})
		case 'C':
			changed = append(changed, changedEntry{path: paths[1], mode: dstMode, blob: dstBlob})
		case 'A', 'M', 'T':
			changed = append(changed, changedEntry{path: paths[0], mode: dstMode, blob: dstBlob})
		default:
			return nil, nil, fmt.Errorf("unsupported git diff status %q for %q", status, paths[0])
		}
	}
	return changed, deleted, nil
}

// resolveDest maps a repository-relative path onto destDir, refusing anything
// that would land outside it. Repository contents come from the VM the agent
// controls, so a crafted path or a symlinked directory component must not turn
// an extraction into a write anywhere on the host.
func resolveDest(destDir string, rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the destination directory", rel)
	}

	// Walk the parent components. destDir itself is the root of the check, so
	// reaching it through a symlink (/tmp on macOS, for instance) is fine;
	// anything below it must be a real directory.
	current := destDir
	for _, part := range strings.Split(filepath.Dir(clean), string(filepath.Separator)) {
		if part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			// The remaining components cannot exist either, and MkdirAll
			// creates real directories for them.
			break
		}
		if err != nil {
			return "", fmt.Errorf("inspect %s: %w", rel, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("path %q crosses symlink %s", rel, part)
		}
	}

	return filepath.Join(destDir, clean), nil
}

func parseFileList(output string) []string {
	files := []string{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files
}
