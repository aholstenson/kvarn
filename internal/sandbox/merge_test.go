package sandbox_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/aholstenson/kvarn/internal/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The merge is defined by what git does with two histories, so these specs run
// it against a real repository through the same fake runner the extraction
// specs use.
var _ = Describe("Merging in the guest", func() {
	var (
		ctx      context.Context
		runner   *gitRunnerProxy
		repoDir  string
		identity = sandbox.Identity{Name: "kvarn", Email: "kvarn@localhost"}
	)

	// mainRef is spelled the way the tools spell it: the base branch as a ref
	// rather than a name, so nothing can resolve it to a tag or a file.
	const mainRef = "refs/heads/main"

	BeforeEach(func() {
		ctx = context.Background()
		runner = newGitRunner()

		repoDir = filepath.Join(GinkgoT().TempDir(), "workspace")
		Expect(os.MkdirAll(repoDir, 0o755)).To(Succeed())

		git(repoDir, "init", "-b", "main")
		git(repoDir, "config", "user.email", "test@example.com")
		git(repoDir, "config", "user.name", "Test")

		writeFile(repoDir, "shared.txt", "one\n", 0o644)
		writeFile(repoDir, "untouched.txt", "steady\n", 0o644)
		git(repoDir, "add", "-A")
		git(repoDir, "commit", "-m", "initial")

		// The pull request's head branch, which then falls behind.
		git(repoDir, "checkout", "-b", "feature")
		writeFile(repoDir, "feature.txt", "feature\n", 0o644)
		git(repoDir, "add", "-A")
		git(repoDir, "commit", "-m", "feature work")

		git(repoDir, "checkout", "main")
		writeFile(repoDir, "base.txt", "base\n", 0o644)
		git(repoDir, "add", "-A")
		git(repoDir, "commit", "-m", "base moves on")

		git(repoDir, "checkout", "feature")
	})

	// conflictOnShared makes both branches change the same line, which is what
	// git cannot decide on its own.
	conflictOnShared := func() {
		git(repoDir, "checkout", "main")
		writeFile(repoDir, "shared.txt", "from base\n", 0o644)
		git(repoDir, "add", "-A")
		git(repoDir, "commit", "-m", "base edits shared")

		git(repoDir, "checkout", "feature")
		writeFile(repoDir, "shared.txt", "from feature\n", 0o644)
		git(repoDir, "add", "-A")
		git(repoDir, "commit", "-m", "feature edits shared")
	}

	Describe("StartMerge", func() {
		It("stages a clean merge without committing it", func() {
			status, err := sandbox.StartMerge(ctx, runner, repoDir, mainRef)
			Expect(err).NotTo(HaveOccurred())
			Expect(status.AlreadyUpToDate).To(BeFalse())
			Expect(status.Conflicts).To(BeEmpty())

			// Staged, not committed: the commit boundary is finish_merge's.
			Expect(sandbox.MergeInProgress(ctx, runner, repoDir)).To(BeTrue())
			Expect(filepath.Join(repoDir, "base.txt")).To(BeAnExistingFile())
			Expect(gitOut(repoDir, "log", "-1", "--format=%s")).To(Equal("feature work"))
		})

		It("reports the paths git could not merge", func() {
			conflictOnShared()

			status, err := sandbox.StartMerge(ctx, runner, repoDir, mainRef)
			Expect(err).NotTo(HaveOccurred())
			Expect(status.Conflicts).To(Equal([]string{"shared.txt"}))

			content, err := os.ReadFile(filepath.Join(repoDir, "shared.txt"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(ContainSubstring("<<<<<<< "))
		})

		It("says so when the branch is already merged", func() {
			git(repoDir, "merge", "--no-edit", "main")

			status, err := sandbox.StartMerge(ctx, runner, repoDir, mainRef)
			Expect(err).NotTo(HaveOccurred())
			Expect(status.AlreadyUpToDate).To(BeTrue())
			Expect(sandbox.MergeInProgress(ctx, runner, repoDir)).To(BeFalse())
		})

		It("fails rather than reporting a conflict when the ref is unknown", func() {
			_, err := sandbox.StartMerge(ctx, runner, repoDir, "refs/heads/nope")
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("FinishMerge", func() {
		It("commits the merge with both parents", func() {
			head := gitOut(repoDir, "rev-parse", "HEAD")
			base := gitOut(repoDir, "rev-parse", "main")

			_, err := sandbox.StartMerge(ctx, runner, repoDir, mainRef)
			Expect(err).NotTo(HaveOccurred())

			sha, err := sandbox.FinishMerge(ctx, runner, repoDir, identity, "Merge branch 'main' into 'feature'")
			Expect(err).NotTo(HaveOccurred())

			Expect(gitOut(repoDir, "rev-list", "--parents", "-n", "1", sha)).
				To(Equal(strings.Join([]string{sha, head, base}, " ")))
			Expect(gitOut(repoDir, "log", "-1", "--format=%an <%ae>")).To(Equal("kvarn <kvarn@localhost>"))
			Expect(sandbox.MergeInProgress(ctx, runner, repoDir)).To(BeFalse())
		})

		It("commits a resolved conflict", func() {
			conflictOnShared()
			_, err := sandbox.StartMerge(ctx, runner, repoDir, mainRef)
			Expect(err).NotTo(HaveOccurred())

			writeFile(repoDir, "shared.txt", "from both\n", 0o644)

			sha, err := sandbox.FinishMerge(ctx, runner, repoDir, identity, "Merge branch 'main' into 'feature'")
			Expect(err).NotTo(HaveOccurred())
			Expect(gitOut(repoDir, "show", sha+":shared.txt")).To(Equal("from both"))
		})

		It("refuses a conflict the agent left untouched", func() {
			conflictOnShared()
			_, err := sandbox.StartMerge(ctx, runner, repoDir, mainRef)
			Expect(err).NotTo(HaveOccurred())

			_, err = sandbox.FinishMerge(ctx, runner, repoDir, identity, "Merge branch 'main' into 'feature'")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("conflict markers"))
			Expect(err.Error()).To(ContainSubstring("shared.txt"))
			Expect(sandbox.MergeInProgress(ctx, runner, repoDir)).To(BeTrue())
		})

		It("refuses a marker left in a file the agent did edit", func() {
			conflictOnShared()
			_, err := sandbox.StartMerge(ctx, runner, repoDir, mainRef)
			Expect(err).NotTo(HaveOccurred())

			// Edited, so the index no longer calls it unmerged once staged — and
			// still half-resolved.
			writeFile(repoDir, "shared.txt", "kept both:\n<<<<<<< HEAD\nfrom feature\n", 0o644)
			git(repoDir, "add", "shared.txt")

			_, err = sandbox.FinishMerge(ctx, runner, repoDir, identity, "Merge branch 'main' into 'feature'")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("conflict markers"))
			Expect(err.Error()).To(ContainSubstring("shared.txt"))
		})

		It("refuses a merge that moves a submodule", func() {
			// A repository inside the worktree, referenced from the index as a
			// gitlink — the shape a submodule bump has, and the one extraction
			// cannot carry out of the VM.
			subDir := filepath.Join(repoDir, "sub")
			Expect(os.MkdirAll(subDir, 0o755)).To(Succeed())
			git(subDir, "init", "-b", "main")
			git(subDir, "config", "user.email", "test@example.com")
			git(subDir, "config", "user.name", "Test")
			writeFile(subDir, "inner.txt", "inner\n", 0o644)
			git(subDir, "add", "-A")
			git(subDir, "commit", "-m", "inner")
			subSHA := gitOut(subDir, "rev-parse", "HEAD")

			_, err := sandbox.StartMerge(ctx, runner, repoDir, mainRef)
			Expect(err).NotTo(HaveOccurred())
			git(repoDir, "update-index", "--add", "--cacheinfo", "160000,"+subSHA+",sub")

			_, err = sandbox.FinishMerge(ctx, runner, repoDir, identity, "Merge branch 'main' into 'feature'")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("submodule"))
			Expect(err.Error()).To(ContainSubstring("sub"))
		})

		It("refuses when no merge is in progress", func() {
			_, err := sandbox.FinishMerge(ctx, runner, repoDir, identity, "Merge branch 'main' into 'feature'")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no merge is in progress"))
		})
	})
})
