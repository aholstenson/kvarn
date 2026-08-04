package git_test

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/scm"
	scmgit "github.com/aholstenson/kvarn/internal/scm/git"
)

// The clone kvarn makes is shipped into a VM, so anything git records about
// where it came from travels with it. These specs pin that down by cloning
// from a path that carries a canary where a credential would sit in a real
// URL, then scanning every byte of the resulting repository for it.
var _ = Describe("Credential containment", func() {
	const canary = "SUPERSECRET-CANARY"

	var (
		g       *scmgit.Git
		tmpDir  string
		bareDir string
	)

	BeforeEach(func() {
		g = &scmgit.Git{}
		tmpDir = GinkgoT().TempDir()

		// The canary sits in the clone URL itself, which is where an embedded
		// credential would be.
		bareDir = filepath.Join(tmpDir, canary, "bare.git")
		Expect(os.MkdirAll(filepath.Dir(bareDir), 0o755)).To(Succeed())

		workDir := filepath.Join(tmpDir, "work")
		runOK("git", "init", "--bare", bareDir)
		runOK("git", "clone", bareDir, workDir)
		Expect(os.WriteFile(filepath.Join(workDir, "hello.txt"), []byte("hello\n"), 0o644)).To(Succeed())
		runIn(workDir, "git", "add", "hello.txt")
		runIn(workDir, "git", "-c", "user.email=t@t", "-c", "user.name=T", "commit", "-m", "initial")
		runIn(workDir, "git", "push", "origin", "HEAD")
	})

	It("leaves no trace of the clone URL anywhere in the repository", func() {
		dest := filepath.Join(tmpDir, "clone")
		Expect(g.Clone(context.Background(), scm.CloneOpts{
			URL:         bareDir,
			Destination: dest,
		})).To(Succeed())

		var offenders []string
		Expect(filepath.WalkDir(filepath.Join(dest, ".git"), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if strings.Contains(string(data), canary) {
				rel, _ := filepath.Rel(dest, path)
				offenders = append(offenders, rel)
			}
			return nil
		})).To(Succeed())
		Expect(offenders).To(BeEmpty())
	})

	It("leaves no remote behind for a credential to hide in", func() {
		dest := filepath.Join(tmpDir, "clone-noremote")
		Expect(g.Clone(context.Background(), scm.CloneOpts{
			URL:         bareDir,
			Destination: dest,
		})).To(Succeed())

		config, err := os.ReadFile(filepath.Join(dest, ".git", "config"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(config)).NotTo(ContainSubstring("[remote"))
		Expect(string(config)).NotTo(ContainSubstring("credential"))

		out := outputIn(dest, "git", "remote")
		Expect(strings.TrimSpace(out)).To(BeEmpty())

		// The reflog is the other place git writes the clone URL, as the
		// "clone: from <url>" entry.
		_, err = os.Stat(filepath.Join(dest, ".git", "logs"))
		Expect(os.IsNotExist(err)).To(BeTrue())
	})

	It("keeps the repository usable without its remote", func() {
		dest := filepath.Join(tmpDir, "clone-usable")
		Expect(g.Clone(context.Background(), scm.CloneOpts{
			URL:         bareDir,
			Destination: dest,
		})).To(Succeed())

		Expect(outputIn(dest, "git", "log", "-1", "--format=%s")).To(ContainSubstring("initial"))
		content, err := os.ReadFile(filepath.Join(dest, "hello.txt"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(content)).To(Equal("hello\n"))
	})

	It("rebuilds its worktree from the repository alone", func() {
		// What the sandbox does: only .git crosses the transport, and the
		// guest runs `reset --hard HEAD` to write the files. Assert here, on
		// the host, that a clone survives losing its worktree — the whole
		// premise of shipping half as many bytes.
		dest := filepath.Join(tmpDir, "clone-repo-only")
		Expect(g.Clone(context.Background(), scm.CloneOpts{
			URL:         bareDir,
			Destination: dest,
		})).To(Succeed())

		guest := filepath.Join(tmpDir, "guest")
		Expect(os.MkdirAll(guest, 0o755)).To(Succeed())
		runOK("cp", "-a", filepath.Join(dest, ".git"), filepath.Join(guest, ".git"))
		Expect(filepath.Join(guest, "hello.txt")).NotTo(BeAnExistingFile())

		runIn(guest, "git",
			"-c", "filter.lfs.smudge=cat",
			"-c", "filter.lfs.required=false",
			"reset", "--hard", "HEAD")

		content, err := os.ReadFile(filepath.Join(guest, "hello.txt"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(content)).To(Equal("hello\n"))
		// A clean tree afterwards is what keeps ChangedFiles honest: anything
		// the agent has not touched must not show up as a change.
		Expect(strings.TrimSpace(outputIn(guest, "git", "status", "--porcelain"))).To(BeEmpty())
	})

	It("passes the token through the environment, never the command line", func() {
		auth, err := scmgit.ResolveAuth(context.Background(), "https://example.com/x.git",
			scm.StaticCredentials(&scm.Credentials{Token: canary}))
		Expect(err).NotTo(HaveOccurred())
		defer auth.Close()

		for _, c := range auth.Config {
			Expect(c).NotTo(ContainSubstring(canary))
		}
		Expect(strings.Join(auth.Env, "\n")).To(ContainSubstring(canary))
		// The helper chain is reset so a host-configured helper can neither
		// supply nor capture credentials.
		Expect(auth.Config[0]).To(Equal("credential.helper="))
	})

	It("keeps a credentialed URL out of anything printed about it", func() {
		// A log file outlives the job that wrote it, so a URL on its way to one
		// is subject to the same rule as the clone itself.
		Expect(scmgit.RedactURL("https://user:" + canary + "@example.com/org/repo.git")).
			To(Equal("https://redacted@example.com/org/repo.git"))
		// A token spelled as the username leaks just as well.
		Expect(scmgit.RedactURL("https://" + canary + "@example.com/org/repo.git")).
			To(Equal("https://redacted@example.com/org/repo.git"))
		// Forms url.Parse rejects still must not print their password.
		Expect(scmgit.RedactURL("https://u:" + canary + "@exa mple.com/repo.git")).
			NotTo(ContainSubstring(canary))
		Expect(scmgit.RedactURL("git:" + canary + "@github.com:org/repo.git")).
			NotTo(ContainSubstring(canary))
	})

	It("leaves a URL without a credential legible", func() {
		Expect(scmgit.RedactURL("https://github.com/org/repo.git")).
			To(Equal("https://github.com/org/repo.git"))
		Expect(scmgit.RedactURL("git@github.com:org/repo.git")).
			To(Equal("git@github.com:org/repo.git"))
		Expect(scmgit.RedactURL("/var/cache/kvarn/repos/demo/mirror.git")).
			To(Equal("/var/cache/kvarn/repos/demo/mirror.git"))
	})

	It("rejects an auth method the URL cannot use", func() {
		_, err := scmgit.ResolveAuth(context.Background(), "git@example.com:org/repo.git",
			scm.StaticCredentials(&scm.Credentials{Token: "t"}))
		Expect(err).To(MatchError(ContainSubstring("auth method mismatch")))

		_, err = scmgit.ResolveAuth(context.Background(), "https://example.com/x.git",
			scm.StaticCredentials(&scm.Credentials{SSHKey: []byte("-----BEGIN OPENSSH PRIVATE KEY-----")}))
		Expect(err).To(MatchError(ContainSubstring("auth method mismatch")))
	})
})

func runOK(name string, args ...string) {
	GinkgoHelper()
	out, err := exec.Command(name, args...).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), string(out))
}

func runIn(dir string, name string, args ...string) {
	GinkgoHelper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), string(out))
}

func outputIn(dir string, name string, args ...string) string {
	GinkgoHelper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	Expect(err).NotTo(HaveOccurred())
	return string(out)
}
