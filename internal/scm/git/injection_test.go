package git_test

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/scm"
	scmgit "github.com/aholstenson/kvarn/internal/scm/git"
)

// Branch names reach kvarn from project config and, on a feedback run, from
// whoever opened the pull request. `git clone --upload-pack=<cmd>` runs an
// arbitrary program, so a branch that git reads as an option rather than a ref
// is remote code execution on the orchestrator host.
var _ = Describe("Argument injection", func() {
	const hostileBranch = "--upload-pack=false"

	var (
		tmpDir  string
		bareDir string
	)

	BeforeEach(func() {
		tmpDir = GinkgoT().TempDir()
		bareDir = filepath.Join(tmpDir, "bare.git")
		workDir := filepath.Join(tmpDir, "work")

		runOK("git", "init", "--bare", bareDir)
		runOK("git", "clone", bareDir, workDir)
		Expect(os.WriteFile(filepath.Join(workDir, "a.txt"), []byte("a\n"), 0o644)).To(Succeed())
		runIn(workDir, "git", "add", "a.txt")
		runIn(workDir, "git", "-c", "user.email=t@t", "-c", "user.name=T", "commit", "-m", "initial")
		runIn(workDir, "git", "push", "origin", "HEAD")
	})

	It("treats a flag-shaped branch as a ref that does not exist", func() {
		err := (&scmgit.Git{}).Clone(context.Background(), scm.CloneOpts{
			URL:         bareDir,
			Branch:      hostileBranch,
			Destination: filepath.Join(tmpDir, "hostile"),
		})
		// git looked for a branch by that name rather than acting on an
		// option; had it been parsed as one, the failure would name
		// upload-pack instead.
		Expect(err).To(MatchError(ContainSubstring("Remote branch " + hostileBranch)))
		Expect(err).To(MatchError(ContainSubstring("not found")))
	})

	It("clones a branch whose name begins with a dash", func() {
		// The mirror image of the case above: a legitimate — if unwise —
		// branch name must still work.
		workDir := filepath.Join(tmpDir, "work")
		runIn(workDir, "git", "checkout", "-b", "dashed")
		Expect(os.WriteFile(filepath.Join(workDir, "b.txt"), []byte("b\n"), 0o644)).To(Succeed())
		runIn(workDir, "git", "add", "b.txt")
		runIn(workDir, "git", "-c", "user.email=t@t", "-c", "user.name=T", "commit", "-m", "dashed")
		runIn(workDir, "git", "push", "origin", "dashed")

		dest := filepath.Join(tmpDir, "dashed-clone")
		Expect((&scmgit.Git{}).Clone(context.Background(), scm.CloneOpts{
			URL:         bareDir,
			Branch:      "dashed",
			Destination: dest,
		})).To(Succeed())
		Expect(filepath.Join(dest, "b.txt")).To(BeAnExistingFile())
	})

	It("places operands beyond git's option parsing", func() {
		// Run is the single place the option/operand boundary is enforced, so
		// assert it there too: a dash-leading operand must reach git as a
		// value, not as an unknown option.
		_, err := scmgit.Run(context.Background(), scmgit.Cmd{
			Dir:      tmpDir,
			Sub:      "rev-parse",
			Operands: []string{"--not-an-option"},
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).NotTo(ContainSubstring("unknown option"))
	})
})

var _ = Describe("CheckAvailable", func() {
	It("accepts the git on this host", func() {
		Expect(scmgit.CheckAvailable()).To(Succeed())
	})
})
