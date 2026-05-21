package transfer

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitIgnoreFilter returns a SkipFile function that skips files not tracked by
// git and files matching .gitignore rules. It uses `git ls-files` to determine
// which files should be included.
//
// If the directory is not a git repository or git is not available, it returns
// nil (no filtering) without an error.
func GitIgnoreFilter(dir string) (func(relPath string, isDir bool) bool, error) {
	// Check if this is a git repo.
	if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
		return nil, nil
	}

	gitPath, err := exec.LookPath("git")
	if err != nil {
		slog.Warn("git not found on PATH, skipping gitignore filtering")
		return nil, nil
	}

	// Get all tracked and untracked-but-not-ignored files.
	cmd := exec.Command(gitPath, "-C", dir, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, err
	}

	// Build set of allowed files and directory prefixes.
	allowedFiles := make(map[string]struct{})
	allowedDirs := make(map[string]struct{})

	for _, entry := range strings.Split(stdout.String(), "\x00") {
		if entry == "" {
			continue
		}
		// Normalize to OS path separators.
		entry = filepath.FromSlash(entry)
		allowedFiles[entry] = struct{}{}

		// Add all parent directories as allowed.
		dir := filepath.Dir(entry)
		for dir != "." && dir != "" {
			allowedDirs[dir] = struct{}{}
			dir = filepath.Dir(dir)
		}
	}

	return func(relPath string, isDir bool) bool {
		// Never skip the root directory itself.
		if relPath == "." {
			return false
		}

		// Always include the .git directory so the full repository
		// history is available in the VM.
		if relPath == ".git" || strings.HasPrefix(relPath, ".git"+string(filepath.Separator)) {
			return false
		}

		if isDir {
			_, ok := allowedDirs[relPath]
			return !ok
		}

		_, ok := allowedFiles[relPath]
		return !ok
	}, nil
}
