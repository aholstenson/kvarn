package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// gitRunnerProxy stands in for a VM by running the extraction's git commands
// against a directory on the host. Extraction is defined by what git reports,
// so a real repository is the only way to cover modes, symlinks and renames.
type gitRunnerProxy struct {
	*mockRunnerProxy
}

func newGitRunner() *gitRunnerProxy {
	return &gitRunnerProxy{mockRunnerProxy: newMockRunner()}
}

func (g *gitRunnerProxy) Exec(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
	cmd := exec.Command(req.Command, req.Args...)
	cmd.Dir = req.WorkingDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	var exitCode int32
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return nil, err
		}
		exitCode = int32(ee.ExitCode())
	}

	return &v1.ExecResponse{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}

func (g *gitRunnerProxy) StreamFromGuest(_ context.Context, srcPath string, dest io.Writer) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(dest, f)
	return err
}

// git runs a git command in dir and fails the spec if it does not succeed.
func git(dir string, args ...string) {
	GinkgoHelper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "git %v: %s", args, out)
}

// gitOut runs a git command in dir and returns its trimmed stdout.
func gitOut(dir string, args ...string) string {
	GinkgoHelper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	Expect(err).NotTo(HaveOccurred(), "git %v", args)
	return strings.TrimSpace(string(out))
}

// writeFile writes content at path relative to dir, creating parents.
func writeFile(dir string, path string, content string, perm os.FileMode) {
	GinkgoHelper()
	full := filepath.Join(dir, path)
	Expect(os.MkdirAll(filepath.Dir(full), 0o755)).To(Succeed())
	Expect(os.WriteFile(full, []byte(content), perm)).To(Succeed())
	// WriteFile only applies perm when creating, and it is masked by the
	// umask either way.
	Expect(os.Chmod(full, perm)).To(Succeed())
}

// modeOf returns the permission bits of path, following no symlinks.
func modeOf(path string) os.FileMode {
	GinkgoHelper()
	info, err := os.Lstat(path)
	Expect(err).NotTo(HaveOccurred())
	return info.Mode().Perm()
}

var _ = Describe("ExtractChanges", func() {
	var (
		ctx        context.Context
		runner     *gitRunnerProxy
		vmDir      string
		destDir    string
		baseCommit string
	)

	BeforeEach(func() {
		ctx = context.Background()
		runner = newGitRunner()

		root := GinkgoT().TempDir()
		vmDir = filepath.Join(root, "workspace")
		destDir = filepath.Join(root, "clone")
		Expect(os.MkdirAll(vmDir, 0o755)).To(Succeed())

		git(vmDir, "init", "-b", "main")
		git(vmDir, "config", "user.email", "test@example.com")
		git(vmDir, "config", "user.name", "Test")

		// Baseline commit: the state both sides start from.
		writeFile(vmDir, "plain.txt", "original\n", 0o644)
		writeFile(vmDir, "script.sh", "#!/bin/sh\necho hi\n", 0o644)
		writeFile(vmDir, "already-exec.sh", "#!/bin/sh\n", 0o755)
		writeFile(vmDir, "stale.txt", "remove me\n", 0o644)
		writeFile(vmDir, "docs/old.md", "docs\n", 0o644)
		writeFile(vmDir, "becomes-link.txt", "regular\n", 0o644)
		git(vmDir, "add", "-A")
		git(vmDir, "commit", "-m", "baseline")

		baseCommit = gitOut(vmDir, "rev-parse", "HEAD")

		// destDir plays the host clone sitting at that same commit.
		git(root, "clone", vmDir, destDir)
	})

	It("preserves file modes, symlinks, renames and deletions", func() {
		// Content change, mode untouched.
		writeFile(vmDir, "plain.txt", "updated\n", 0o644)
		// Mode change only, in both directions.
		Expect(os.Chmod(filepath.Join(vmDir, "script.sh"), 0o755)).To(Succeed())
		Expect(os.Chmod(filepath.Join(vmDir, "already-exec.sh"), 0o644)).To(Succeed())
		// New executable file in a new directory.
		writeFile(vmDir, "bin/new.sh", "#!/bin/sh\necho new\n", 0o755)
		// New symlink.
		Expect(os.Symlink("plain.txt", filepath.Join(vmDir, "link.txt"))).To(Succeed())
		// Regular file replaced by a symlink (a type change).
		Expect(os.Remove(filepath.Join(vmDir, "becomes-link.txt"))).To(Succeed())
		Expect(os.Symlink("plain.txt", filepath.Join(vmDir, "becomes-link.txt"))).To(Succeed())
		// Rename and delete.
		git(vmDir, "mv", "docs/old.md", "docs/new.md")
		Expect(os.Remove(filepath.Join(vmDir, "stale.txt"))).To(Succeed())

		Expect(sandbox.ExtractChanges(ctx, runner, vmDir, destDir, baseCommit)).To(Succeed())

		Expect(os.ReadFile(filepath.Join(destDir, "plain.txt"))).To(Equal([]byte("updated\n")))
		Expect(modeOf(filepath.Join(destDir, "plain.txt"))).To(Equal(os.FileMode(0o644)))

		Expect(modeOf(filepath.Join(destDir, "script.sh"))).To(Equal(os.FileMode(0o755)))
		Expect(modeOf(filepath.Join(destDir, "already-exec.sh"))).To(Equal(os.FileMode(0o644)))

		Expect(os.ReadFile(filepath.Join(destDir, "bin/new.sh"))).To(Equal([]byte("#!/bin/sh\necho new\n")))
		Expect(modeOf(filepath.Join(destDir, "bin/new.sh"))).To(Equal(os.FileMode(0o755)))

		Expect(os.Readlink(filepath.Join(destDir, "link.txt"))).To(Equal("plain.txt"))
		Expect(os.Readlink(filepath.Join(destDir, "becomes-link.txt"))).To(Equal("plain.txt"))

		Expect(os.ReadFile(filepath.Join(destDir, "docs/new.md"))).To(Equal([]byte("docs\n")))
		Expect(filepath.Join(destDir, "docs/old.md")).NotTo(BeAnExistingFile())
		Expect(filepath.Join(destDir, "stale.txt")).NotTo(BeAnExistingFile())
	})

	It("leaves the destination with a diff git reads back identically", func() {
		writeFile(vmDir, "script.sh", "#!/bin/sh\necho changed\n", 0o755)
		Expect(os.Symlink("plain.txt", filepath.Join(vmDir, "link.txt"))).To(Succeed())
		Expect(os.Remove(filepath.Join(vmDir, "stale.txt"))).To(Succeed())

		Expect(sandbox.ExtractChanges(ctx, runner, vmDir, destDir, baseCommit)).To(Succeed())

		// The point of preserving modes is that the commit made from destDir
		// matches the VM's tree, so compare what each side would commit.
		git(destDir, "add", "-A")
		vmTree := runTree(vmDir)
		destTree := runTree(destDir)
		Expect(destTree).To(Equal(vmTree))
	})

	It("replaces a symlink with a regular file when the VM did", func() {
		Expect(os.Remove(filepath.Join(vmDir, "plain.txt"))).To(Succeed())
		Expect(os.Symlink("stale.txt", filepath.Join(vmDir, "plain.txt"))).To(Succeed())
		git(vmDir, "add", "-A")
		git(vmDir, "commit", "-m", "make plain.txt a symlink")
		git(destDir, "pull", "--ff-only")
		baseCommit = gitOut(vmDir, "rev-parse", "HEAD")

		Expect(os.Remove(filepath.Join(vmDir, "plain.txt"))).To(Succeed())
		writeFile(vmDir, "plain.txt", "regular again\n", 0o644)

		Expect(sandbox.ExtractChanges(ctx, runner, vmDir, destDir, baseCommit)).To(Succeed())

		info, err := os.Lstat(filepath.Join(destDir, "plain.txt"))
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode() & os.ModeSymlink).To(BeZero())
		Expect(os.ReadFile(filepath.Join(destDir, "plain.txt"))).To(Equal([]byte("regular again\n")))
	})

	It("extracts work the agent committed inside the VM", func() {
		// Agents are free to run git themselves; against HEAD the commit below
		// would look like an untouched worktree and the work would be lost.
		writeFile(vmDir, "plain.txt", "committed\n", 0o644)
		writeFile(vmDir, "bin/new.sh", "#!/bin/sh\n", 0o755)
		Expect(os.Remove(filepath.Join(vmDir, "stale.txt"))).To(Succeed())
		git(vmDir, "add", "-A")
		git(vmDir, "commit", "-m", "agent commit")

		Expect(sandbox.ExtractChanges(ctx, runner, vmDir, destDir, baseCommit)).To(Succeed())

		Expect(os.ReadFile(filepath.Join(destDir, "plain.txt"))).To(Equal([]byte("committed\n")))
		Expect(modeOf(filepath.Join(destDir, "bin/new.sh"))).To(Equal(os.FileMode(0o755)))
		Expect(filepath.Join(destDir, "stale.txt")).NotTo(BeAnExistingFile())
	})

	It("refuses to write through a symlinked directory in the destination", func() {
		outside := GinkgoT().TempDir()
		Expect(os.Symlink(outside, filepath.Join(destDir, "docs-link"))).To(Succeed())
		writeFile(vmDir, "docs-link/pwned.txt", "should not escape\n", 0o644)

		err := sandbox.ExtractChanges(ctx, runner, vmDir, destDir, baseCommit)
		Expect(err).To(MatchError(ContainSubstring("crosses symlink")))
		Expect(filepath.Join(outside, "pwned.txt")).NotTo(BeAnExistingFile())
	})
})

// runTree returns `git write-tree` output for dir's index, identifying the
// exact tree (content plus modes) that a commit from dir would record.
func runTree(dir string) string {
	GinkgoHelper()
	cmd := exec.Command("git", "write-tree")
	cmd.Dir = dir
	out, err := cmd.Output()
	Expect(err).NotTo(HaveOccurred())
	return string(out)
}
