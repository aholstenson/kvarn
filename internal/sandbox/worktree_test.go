package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// hostExecRunner runs Exec requests on this machine, so the worktree scripts
// are tested against a real git rather than a recording of one.
type hostExecRunner struct {
	*mockRunnerProxy
	// env is added to the environment of every command.
	env []string
}

func (r *hostExecRunner) Exec(ctx context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
	cmd := exec.CommandContext(ctx, req.Command, req.Args...)
	cmd.Dir = req.WorkingDir
	if len(r.env) > 0 {
		cmd.Env = append(os.Environ(), r.env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		return &v1.ExecResponse{ExitCode: int32(exitErr.ExitCode()), Stdout: stdout.String(), Stderr: stderr.String()}, nil
	case err != nil:
		return nil, err
	}
	return &v1.ExecResponse{Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

var _ = Describe("Worktree snapshots", func() {
	var (
		dir    string
		runner *hostExecRunner
		ctx    context.Context
	)

	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(out))
	}
	write := func(name, content string) {
		path := filepath.Join(dir, name)
		Expect(os.MkdirAll(filepath.Dir(path), 0o755)).To(Succeed())
		Expect(os.WriteFile(path, []byte(content), 0o644)).To(Succeed())
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		Expect(err).NotTo(HaveOccurred())
		return string(b)
	}

	BeforeEach(func() {
		if _, err := exec.LookPath("git"); err != nil {
			Skip("git is not installed")
		}
		dir = GinkgoT().TempDir()
		runner = &hostExecRunner{mockRunnerProxy: newMockRunner()}
		ctx = context.Background()

		run("init", "-q")
		write(".gitignore", "build/\n")
		write("main.go", "package main\n")
		write("docs/readme.md", "docs\n")
		run("add", "-A")
		run("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "base")

		// Work the coding agent has done and not committed: an edit, a new
		// file, and something staged. The snapshot must keep all of it.
		write("main.go", "package main\n\nfunc main() {}\n")
		write("feature.go", "package main\n")
		run("add", "feature.go")
	})

	It("puts back edits, deletions and new files made after the snapshot", func() {
		tree, err := sandbox.SnapshotWorktree(ctx, runner, dir)
		Expect(err).NotTo(HaveOccurred())

		write("main.go", "broken\n")
		Expect(os.Remove(filepath.Join(dir, "docs/readme.md"))).To(Succeed())
		write("scratch/check.sh", "echo hi\n")
		write("build/out.bin", "ignored\n")

		restored, err := sandbox.RestoreWorktree(ctx, runner, dir, tree)
		Expect(err).NotTo(HaveOccurred())
		Expect(restored).To(BeTrue())

		Expect(read("main.go")).To(Equal("package main\n\nfunc main() {}\n"))
		Expect(read("docs/readme.md")).To(Equal("docs\n"))
		Expect(read("feature.go")).To(Equal("package main\n"))
		Expect(filepath.Join(dir, "scratch")).NotTo(BeADirectory())
		Expect(read("build/out.bin")).To(Equal("ignored\n"), "ignored files are not part of the change")
	})

	It("puts back a tracked file that an ignore pattern matches", func() {
		write("build/committed.txt", "kept\n")
		run("add", "-f", "build/committed.txt")
		run("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "tracked ignored file")

		tree, err := sandbox.SnapshotWorktree(ctx, runner, dir)
		Expect(err).NotTo(HaveOccurred())

		write("build/committed.txt", "changed by the check\n")

		restored, err := sandbox.RestoreWorktree(ctx, runner, dir, tree)
		Expect(err).NotTo(HaveOccurred())
		Expect(restored).To(BeTrue())
		Expect(read("build/committed.txt")).To(Equal("kept\n"))
	})

	It("does not rewrite files the check did not change", func() {
		tree, err := sandbox.SnapshotWorktree(ctx, runner, dir)
		Expect(err).NotTo(HaveOccurred())

		old := time.Now().Add(-time.Hour).Truncate(time.Second)
		untouched := filepath.Join(dir, "docs/readme.md")
		Expect(os.Chtimes(untouched, old, old)).To(Succeed())
		write("main.go", "broken\n")

		_, err = sandbox.RestoreWorktree(ctx, runner, dir, tree)
		Expect(err).NotTo(HaveOccurred())

		info, err := os.Stat(untouched)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.ModTime()).To(BeTemporally("==", old))
	})

	It("leaves the workspace alone when nothing changed", func() {
		tree, err := sandbox.SnapshotWorktree(ctx, runner, dir)
		Expect(err).NotTo(HaveOccurred())

		restored, err := sandbox.RestoreWorktree(ctx, runner, dir, tree)
		Expect(err).NotTo(HaveOccurred())
		Expect(restored).To(BeFalse())
	})

	It("does not change what the workspace has staged", func() {
		statusBefore := gitOutput(dir, "status", "--porcelain")
		tree, err := sandbox.SnapshotWorktree(ctx, runner, dir)
		Expect(err).NotTo(HaveOccurred())
		write("scratch.txt", "x\n")
		_, err = sandbox.RestoreWorktree(ctx, runner, dir, tree)
		Expect(err).NotTo(HaveOccurred())
		Expect(gitOutput(dir, "status", "--porcelain")).To(Equal(statusBefore))
	})
})

func gitOutput(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	Expect(err).NotTo(HaveOccurred())
	return string(out)
}
