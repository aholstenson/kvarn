package git_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/scm"
	scmgit "github.com/aholstenson/kvarn/internal/scm/git"
)

// runGit runs a git command in dir and fails the spec if it does not succeed.
func runGit(dir string, args ...string) string {
	GinkgoHelper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

var _ = Describe("MergeCommit", func() {
	var (
		ctx      context.Context
		tmpDir   string
		bareDir  string
		cloneDir string
		baseTip  string
	)

	BeforeEach(func() {
		ctx = context.Background()
		tmpDir = GinkgoT().TempDir()

		// A repository with a base branch that has moved on since the feature
		// branch left it — the shape of a pull request that has fallen behind.
		bareDir = filepath.Join(tmpDir, "bare.git")
		runGit(tmpDir, "init", "--bare", "-b", "main", bareDir)

		seed := filepath.Join(tmpDir, "seed")
		runGit(tmpDir, "clone", bareDir, seed)
		runGit(seed, "config", "user.email", "test@test.com")
		runGit(seed, "config", "user.name", "Test")

		Expect(os.WriteFile(filepath.Join(seed, "shared.txt"), []byte("one\n"), 0o644)).To(Succeed())
		runGit(seed, "add", "-A")
		runGit(seed, "commit", "-m", "initial")
		runGit(seed, "push", "origin", "main")

		runGit(seed, "checkout", "-b", "feature")
		Expect(os.WriteFile(filepath.Join(seed, "feature.txt"), []byte("feature\n"), 0o644)).To(Succeed())
		runGit(seed, "add", "-A")
		runGit(seed, "commit", "-m", "feature work")
		runGit(seed, "push", "origin", "feature")

		runGit(seed, "checkout", "main")
		Expect(os.WriteFile(filepath.Join(seed, "shared.txt"), []byte("two\n"), 0o644)).To(Succeed())
		runGit(seed, "add", "-A")
		runGit(seed, "commit", "-m", "move base on")
		runGit(seed, "push", "origin", "main")
		baseTip = runGit(seed, "rev-parse", "HEAD")

		// The job clone: the feature branch, with the base branch fetched in the
		// way prepareMergeBase does.
		cloneDir = filepath.Join(tmpDir, "clone")
		runGit(tmpDir, "clone", "--branch", "feature", bareDir, cloneDir)
		Expect(scmgit.FetchBranch(ctx, scmgit.FetchBranchOpts{
			RepoDir: cloneDir,
			Source:  bareDir,
			Branch:  "main",
		})).To(Succeed())
	})

	It("records the worktree with both parents", func() {
		head := runGit(cloneDir, "rev-parse", "HEAD")

		// The worktree as the VM would hand it back: the merged content, with no
		// index or MERGE_HEAD to explain it.
		Expect(os.WriteFile(filepath.Join(cloneDir, "shared.txt"), []byte("two\n"), 0o644)).To(Succeed())

		sha, err := scmgit.MergeCommit(ctx, scmgit.MergeCommitOpts{
			RepoDir:     cloneDir,
			Message:     "Merge branch 'main' into 'feature'",
			MergeParent: baseTip,
			AuthorName:  "kvarn",
			AuthorEmail: "kvarn@noreply",
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(runGit(cloneDir, "rev-list", "--parents", "-n", "1", sha)).
			To(Equal(strings.Join([]string{sha, head, baseTip}, " ")))
		Expect(runGit(cloneDir, "rev-parse", "HEAD")).To(Equal(sha))
		Expect(runGit(cloneDir, "log", "-1", "--format=%s", sha)).
			To(Equal("Merge branch 'main' into 'feature'"))

		// The commit's tree is the worktree it was made from, and the worktree is
		// left clean rather than still showing the change as pending.
		Expect(runGit(cloneDir, "status", "--porcelain")).To(BeEmpty())
		Expect(runGit(cloneDir, "show", sha+":shared.txt")).To(Equal("two"))
		Expect(runGit(cloneDir, "show", sha+":feature.txt")).To(Equal("feature"))
	})

	It("refuses a merge parent the repository does not hold", func() {
		_, err := scmgit.MergeCommit(ctx, scmgit.MergeCommitOpts{
			RepoDir:     cloneDir,
			Message:     "Merge something absent",
			MergeParent: "0123456789012345678901234567890123456789",
			AuthorName:  "kvarn",
			AuthorEmail: "kvarn@noreply",
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("resolve merge parent"))
	})

	It("pushes a recorded commit when the trailing diff is empty", func() {
		sha, err := scmgit.MergeCommit(ctx, scmgit.MergeCommitOpts{
			RepoDir:     cloneDir,
			Message:     "Merge branch 'main' into 'feature'",
			MergeParent: baseTip,
			AuthorName:  "kvarn",
			AuthorEmail: "kvarn@noreply",
		})
		Expect(err).NotTo(HaveOccurred())

		g := &scmgit.Git{}
		Expect(g.CommitAndPush(ctx, scm.CommitAndPushOpts{
			RepoDir:     cloneDir,
			RemoteURL:   bareDir,
			Branch:      "feature",
			Message:     "Address review feedback",
			AuthorName:  "kvarn",
			AuthorEmail: "kvarn@noreply",
		})).To(Succeed())

		// Exactly the merge reached the remote: no empty commit on top of it.
		Expect(runGit(bareDir, "rev-parse", "refs/heads/feature")).To(Equal(sha))
	})

	It("pushes the merge and a trailing commit in that order", func() {
		mergeSHA, err := scmgit.MergeCommit(ctx, scmgit.MergeCommitOpts{
			RepoDir:     cloneDir,
			Message:     "Merge branch 'main' into 'feature'",
			MergeParent: baseTip,
			AuthorName:  "kvarn",
			AuthorEmail: "kvarn@noreply",
		})
		Expect(err).NotTo(HaveOccurred())

		// What the agent did after finishing the merge.
		Expect(os.WriteFile(filepath.Join(cloneDir, "feature.txt"), []byte("fixed\n"), 0o644)).To(Succeed())

		g := &scmgit.Git{}
		Expect(g.CommitAndPush(ctx, scm.CommitAndPushOpts{
			RepoDir:     cloneDir,
			RemoteURL:   bareDir,
			Branch:      "feature",
			Message:     "Fix the thing",
			AuthorName:  "kvarn",
			AuthorEmail: "kvarn@noreply",
		})).To(Succeed())

		tip := runGit(bareDir, "rev-parse", "refs/heads/feature")
		Expect(runGit(bareDir, "log", "-1", "--format=%s", tip)).To(Equal("Fix the thing"))
		Expect(runGit(bareDir, "rev-parse", tip+"^")).To(Equal(mergeSHA))
	})
})

var _ = Describe("Fetching a base branch", func() {
	var (
		ctx     context.Context
		tmpDir  string
		bareDir string
	)

	BeforeEach(func() {
		ctx = context.Background()
		tmpDir = GinkgoT().TempDir()

		bareDir = filepath.Join(tmpDir, "bare.git")
		runGit(tmpDir, "init", "--bare", "-b", "main", bareDir)

		seed := filepath.Join(tmpDir, "seed")
		runGit(tmpDir, "clone", bareDir, seed)
		runGit(seed, "config", "user.email", "test@test.com")
		runGit(seed, "config", "user.name", "Test")

		// Deep enough that a depth-1 clone cannot see where the branches parted.
		for i := 0; i < 4; i++ {
			Expect(os.WriteFile(filepath.Join(seed, "shared.txt"), []byte(strings.Repeat("x", i+1)), 0o644)).To(Succeed())
			runGit(seed, "add", "-A")
			runGit(seed, "commit", "-m", "base work")
		}
		runGit(seed, "push", "origin", "main")

		runGit(seed, "checkout", "-b", "feature", "HEAD~2")
		Expect(os.WriteFile(filepath.Join(seed, "feature.txt"), []byte("feature\n"), 0o644)).To(Succeed())
		runGit(seed, "add", "-A")
		runGit(seed, "commit", "-m", "feature work")
		runGit(seed, "push", "origin", "feature")
	})

	It("finds the merge base once a shallow clone is completed", func() {
		cloneDir := filepath.Join(tmpDir, "shallow")
		runGit(tmpDir, "clone", "--no-local", "--depth", "1", "--branch", "feature", "file://"+bareDir, cloneDir)

		Expect(scmgit.FetchBranch(ctx, scmgit.FetchBranchOpts{
			RepoDir: cloneDir,
			Source:  bareDir,
			Branch:  "main",
		})).To(Succeed())

		Expect(scmgit.IsShallow(ctx, cloneDir)).To(BeTrue())
		Expect(scmgit.HasMergeBase(ctx, cloneDir, "HEAD", "refs/heads/main")).To(BeFalse())

		Expect(scmgit.Unshallow(ctx, scmgit.UnshallowOpts{
			RepoDir: cloneDir,
			Source:  bareDir,
			Refs:    []string{"refs/heads/feature", "refs/heads/main"},
		})).To(Succeed())

		Expect(scmgit.IsShallow(ctx, cloneDir)).To(BeFalse())
		Expect(scmgit.HasMergeBase(ctx, cloneDir, "HEAD", "refs/heads/main")).To(BeTrue())
	})

	It("leaves a complete repository alone", func() {
		cloneDir := filepath.Join(tmpDir, "full")
		runGit(tmpDir, "clone", "--branch", "feature", bareDir, cloneDir)

		Expect(scmgit.Unshallow(ctx, scmgit.UnshallowOpts{
			RepoDir: cloneDir,
			Source:  bareDir,
			Refs:    []string{"refs/heads/feature"},
		})).To(Succeed())
	})
})
