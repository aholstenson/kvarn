package git_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/scm"
	scmgit "github.com/aholstenson/kvarn/internal/scm/git"
)

var _ = Describe("Git SCM", func() {
	var (
		g       *scmgit.Git
		bareDir string
		tmpDir  string
	)

	BeforeEach(func() {
		g = &scmgit.Git{}

		var err error
		tmpDir, err = os.MkdirTemp("", "git-scm-test-*")
		Expect(err).NotTo(HaveOccurred())

		// Create a local bare repo with a commit.
		bareDir = filepath.Join(tmpDir, "bare.git")
		workDir := filepath.Join(tmpDir, "work")

		cmd := exec.Command("git", "init", "--bare", bareDir)
		Expect(cmd.Run()).To(Succeed())

		cmd = exec.Command("git", "clone", bareDir, workDir)
		Expect(cmd.Run()).To(Succeed())

		// Create a file and commit.
		Expect(os.WriteFile(filepath.Join(workDir, "hello.txt"), []byte("hello world\n"), 0644)).To(Succeed())

		cmd = exec.Command("git", "add", "hello.txt")
		cmd.Dir = workDir
		Expect(cmd.Run()).To(Succeed())

		cmd = exec.Command("git", "-c", "user.email=test@test.com", "-c", "user.name=Test", "commit", "-m", "initial commit")
		cmd.Dir = workDir
		Expect(cmd.Run()).To(Succeed())

		cmd = exec.Command("git", "push", "origin", "HEAD")
		cmd.Dir = workDir
		Expect(cmd.Run()).To(Succeed())

		// Create a second branch.
		cmd = exec.Command("git", "checkout", "-b", "feature")
		cmd.Dir = workDir
		Expect(cmd.Run()).To(Succeed())

		Expect(os.WriteFile(filepath.Join(workDir, "feature.txt"), []byte("feature\n"), 0644)).To(Succeed())

		cmd = exec.Command("git", "add", "feature.txt")
		cmd.Dir = workDir
		Expect(cmd.Run()).To(Succeed())

		cmd = exec.Command("git", "-c", "user.email=test@test.com", "-c", "user.name=Test", "commit", "-m", "feature commit")
		cmd.Dir = workDir
		Expect(cmd.Run()).To(Succeed())

		cmd = exec.Command("git", "push", "origin", "feature")
		cmd.Dir = workDir
		Expect(cmd.Run()).To(Succeed())
	})

	AfterEach(func() {
		os.RemoveAll(tmpDir)
	})

	It("clones a local repo", func() {
		dest := filepath.Join(tmpDir, "clone")
		err := g.Clone(context.Background(), scm.CloneOpts{
			URL:         bareDir,
			Destination: dest,
		})
		Expect(err).NotTo(HaveOccurred())

		content, err := os.ReadFile(filepath.Join(dest, "hello.txt"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(content)).To(Equal("hello world\n"))
	})

	It("clones a specific branch", func() {
		dest := filepath.Join(tmpDir, "clone-branch")
		err := g.Clone(context.Background(), scm.CloneOpts{
			URL:         bareDir,
			Branch:      "feature",
			Destination: dest,
		})
		Expect(err).NotTo(HaveOccurred())

		content, err := os.ReadFile(filepath.Join(dest, "feature.txt"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(content)).To(Equal("feature\n"))
	})

	It("clones with shallow depth", func() {
		dest := filepath.Join(tmpDir, "clone-shallow")
		err := g.Clone(context.Background(), scm.CloneOpts{
			URL:         bareDir,
			Destination: dest,
			Depth:       1,
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify the clone worked.
		content, err := os.ReadFile(filepath.Join(dest, "hello.txt"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(content)).To(Equal("hello world\n"))
	})

	It("returns error for empty URL", func() {
		err := g.Clone(context.Background(), scm.CloneOpts{
			Destination: filepath.Join(tmpDir, "empty"),
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("URL is required"))
	})

	It("returns error for empty destination", func() {
		err := g.Clone(context.Background(), scm.CloneOpts{
			URL: bareDir,
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("destination is required"))
	})

	It("returns error for non-existent repo", func() {
		err := g.Clone(context.Background(), scm.CloneOpts{
			URL:         "/nonexistent/repo.git",
			Destination: filepath.Join(tmpDir, "bad"),
		})
		Expect(err).To(HaveOccurred())
	})

	Describe("Commit and Push", func() {
		var cloneDir string

		BeforeEach(func() {
			// Clone the bare repo to get a working copy.
			cloneDir = filepath.Join(tmpDir, "push-clone")
			err := g.Clone(context.Background(), scm.CloneOpts{
				URL:         bareDir,
				Destination: cloneDir,
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("commits dirty worktree and pushes to new branch", func() {
			// Write a new file to simulate ExtractChanges dirtying the worktree.
			Expect(os.WriteFile(filepath.Join(cloneDir, "new-file.txt"), []byte("new content\n"), 0644)).To(Succeed())

			err := commitAndPush(context.Background(), g, commitPushOpts{
				RepoDir:     cloneDir,
				RemoteURL:   bareDir,
				Branch:      "kvarn/test-session",
				Message:     "test commit",
				AuthorName:  "kvarn",
				AuthorEmail: "kvarn@noreply",
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify the branch exists on the bare remote.
			verifyDir := filepath.Join(tmpDir, "verify")
			err = g.Clone(context.Background(), scm.CloneOpts{
				URL:         bareDir,
				Branch:      "kvarn/test-session",
				Destination: verifyDir,
			})
			Expect(err).NotTo(HaveOccurred())

			content, err := os.ReadFile(filepath.Join(verifyDir, "new-file.txt"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(Equal("new content\n"))
		})

		It("commits modified and deleted files", func() {
			// Modify an existing file.
			Expect(os.WriteFile(filepath.Join(cloneDir, "hello.txt"), []byte("modified\n"), 0644)).To(Succeed())
			// Delete a file won't show as a change since feature.txt isn't on master.
			// Add a new file and also remove hello.txt to test deletions.
			Expect(os.WriteFile(filepath.Join(cloneDir, "added.txt"), []byte("added\n"), 0644)).To(Succeed())

			err := commitAndPush(context.Background(), g, commitPushOpts{
				RepoDir:     cloneDir,
				RemoteURL:   bareDir,
				Branch:      "kvarn/modify-test",
				Message:     "modify and add files",
				AuthorName:  "kvarn",
				AuthorEmail: "kvarn@noreply",
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify on the remote.
			verifyDir := filepath.Join(tmpDir, "verify-modify")
			err = g.Clone(context.Background(), scm.CloneOpts{
				URL:         bareDir,
				Branch:      "kvarn/modify-test",
				Destination: verifyDir,
			})
			Expect(err).NotTo(HaveOccurred())

			content, err := os.ReadFile(filepath.Join(verifyDir, "hello.txt"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(Equal("modified\n"))

			content, err = os.ReadFile(filepath.Join(verifyDir, "added.txt"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(Equal("added\n"))
		})

		It("sets the correct commit author", func() {
			Expect(os.WriteFile(filepath.Join(cloneDir, "author-test.txt"), []byte("test\n"), 0644)).To(Succeed())

			err := commitAndPush(context.Background(), g, commitPushOpts{
				RepoDir:     cloneDir,
				RemoteURL:   bareDir,
				Branch:      "kvarn/author-test",
				Message:     "author test commit",
				AuthorName:  "kvarn",
				AuthorEmail: "kvarn@noreply",
			})
			Expect(err).NotTo(HaveOccurred())

			// Check the commit author on the remote.
			verifyDir := filepath.Join(tmpDir, "verify-author")
			cmd := exec.Command("git", "clone", "--branch", "kvarn/author-test", bareDir, verifyDir)
			Expect(cmd.Run()).To(Succeed())

			cmd = exec.Command("git", "log", "-1", "--format=%an <%ae>")
			cmd.Dir = verifyDir
			out, err := cmd.Output()
			Expect(err).NotTo(HaveOccurred())
			Expect(string(out)).To(ContainSubstring("kvarn <kvarn@noreply>"))
		})

		It("pushes without committing when nothing is staged", func() {
			head := runGit(cloneDir, "rev-parse", "HEAD")

			err := commitAndPush(context.Background(), g, commitPushOpts{
				RepoDir:     cloneDir,
				RemoteURL:   bareDir,
				Branch:      "kvarn/empty",
				Message:     "empty commit",
				AuthorName:  "kvarn",
				AuthorEmail: "kvarn@noreply",
			})
			Expect(err).NotTo(HaveOccurred())

			// The branch is pushed at the commit it already had: a run whose only
			// work is a commit made earlier still has something to deliver, and an
			// empty commit on top would say nothing.
			Expect(runGit(bareDir, "rev-parse", "refs/heads/kvarn/empty")).To(Equal(head))
		})

		It("returns the commit the branch points at", func() {
			Expect(os.WriteFile(filepath.Join(cloneDir, "sha.txt"), []byte("x\n"), 0o644)).To(Succeed())

			sha, err := g.Commit(context.Background(), scm.CommitOpts{
				RepoDir:     cloneDir,
				Branch:      "kvarn/sha",
				Message:     "sha commit",
				AuthorName:  "kvarn",
				AuthorEmail: "kvarn@noreply",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(sha).To(Equal(runGit(cloneDir, "rev-parse", "refs/heads/kvarn/sha")))
		})

		It("pushes the same commit again without an error", func() {
			Expect(os.WriteFile(filepath.Join(cloneDir, "again.txt"), []byte("x\n"), 0o644)).To(Succeed())
			Expect(commitAndPush(context.Background(), g, commitPushOpts{
				RepoDir:     cloneDir,
				RemoteURL:   bareDir,
				Branch:      "kvarn/again",
				Message:     "again",
				AuthorName:  "kvarn",
				AuthorEmail: "kvarn@noreply",
			})).To(Succeed())

			// A delivery repeats a push whose result it never learned, so a push
			// that already landed must succeed again rather than fail the retry.
			Expect(g.Push(context.Background(), scm.PushOpts{
				RepoDir:   cloneDir,
				RemoteURL: bareDir,
				Branch:    "kvarn/again",
			})).To(Succeed())
		})

		It("reports a rejected push as permanent", func() {
			branch := runGit(cloneDir, "rev-parse", "--abbrev-ref", "HEAD")
			Expect(os.WriteFile(filepath.Join(cloneDir, "ours.txt"), []byte("ours\n"), 0o644)).To(Succeed())
			_, err := g.Commit(context.Background(), scm.CommitOpts{
				RepoDir:     cloneDir,
				Branch:      branch,
				Message:     "ours",
				AuthorName:  "kvarn",
				AuthorEmail: "kvarn@noreply",
			})
			Expect(err).NotTo(HaveOccurred())

			// Somebody else moves the remote branch first.
			otherDir := filepath.Join(tmpDir, "other")
			runGit(tmpDir, "clone", bareDir, otherDir)
			Expect(os.WriteFile(filepath.Join(otherDir, "theirs.txt"), []byte("theirs\n"), 0o644)).To(Succeed())
			runGit(otherDir, "add", "-A")
			runGit(otherDir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-m", "theirs")
			runGit(otherDir, "push", "origin", "HEAD")

			err = g.Push(context.Background(), scm.PushOpts{
				RepoDir:   cloneDir,
				RemoteURL: bareDir,
				Branch:    branch,
			})
			Expect(err).To(HaveOccurred())
			Expect(scmgit.IsTransientPushError(err)).To(BeFalse())
		})

		It("reports a push to an unreachable remote as transient", func() {
			err := g.Push(context.Background(), scm.PushOpts{
				RepoDir:   cloneDir,
				RemoteURL: "https://127.0.0.1:1/nothing.git",
				Branch:    runGit(cloneDir, "rev-parse", "--abbrev-ref", "HEAD"),
			})
			Expect(err).To(HaveOccurred())
			Expect(scmgit.IsTransientPushError(err)).To(BeTrue())
		})

		It("returns error for missing repo dir", func() {
			err := commitAndPush(context.Background(), g, commitPushOpts{
				RemoteURL: bareDir,
				Branch:    "kvarn/test",
				Message:   "test",
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("repo dir is required"))
		})

		It("returns error for missing branch", func() {
			err := commitAndPush(context.Background(), g, commitPushOpts{
				RepoDir:   cloneDir,
				RemoteURL: bareDir,
				Message:   "test",
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("branch is required"))
		})

		It("returns error for missing message", func() {
			err := commitAndPush(context.Background(), g, commitPushOpts{
				RepoDir:   cloneDir,
				RemoteURL: bareDir,
				Branch:    "kvarn/test",
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("commit message is required"))
		})
	})
})

// commitPushOpts is what a delivery passes to Commit and then to Push.
type commitPushOpts struct {
	RepoDir     string
	RemoteURL   string
	Branch      string
	Message     string
	AuthorName  string
	AuthorEmail string
}

// commitAndPush commits the worktree and pushes the branch, the way a delivery
// does with its two steps.
func commitAndPush(ctx context.Context, g *scmgit.Git, o commitPushOpts) error {
	if _, err := g.Commit(ctx, scm.CommitOpts{
		RepoDir:     o.RepoDir,
		Branch:      o.Branch,
		Message:     o.Message,
		AuthorName:  o.AuthorName,
		AuthorEmail: o.AuthorEmail,
	}); err != nil {
		return err
	}
	return g.Push(ctx, scm.PushOpts{RepoDir: o.RepoDir, RemoteURL: o.RemoteURL, Branch: o.Branch})
}
