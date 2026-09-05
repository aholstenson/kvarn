package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// homedGitRunner runs the commands for real against a git home of its own.
// The identity is only useful if git accepts how it is spelled, and no mock
// can answer that; the home keeps the run out of the developer's own config.
type homedGitRunner struct {
	*mockProxy
	home string
}

func (r *homedGitRunner) Exec(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
	cmd := exec.Command(req.Command, req.Args...)
	cmd.Dir = req.WorkingDir
	cmd.Env = gitHomeEnv(r.home)

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
	return &v1.ExecResponse{ExitCode: exitCode, Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

// gitHomeEnv points every spelling of git's global config at home, so no
// version of git can reach the one on the machine running the specs.
func gitHomeEnv(home string) []string {
	return append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"GIT_CONFIG_GLOBAL="+filepath.Join(home, ".gitconfig"),
	)
}

// gitGlobal reads a setting back out of that home.
func gitGlobal(home, key string) string {
	cmd := exec.Command("git", "config", "--global", "--get", "--", key)
	cmd.Env = gitHomeEnv(home)
	out, err := cmd.Output()
	Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(string(out))
}

var _ = Describe("ConfigureIdentity", func() {
	var (
		ctx   context.Context
		proxy *mockProxy
	)

	BeforeEach(func() {
		ctx = context.Background()
		proxy = newMockProxy()
	})

	It("writes the identity globally as the unprivileged user", func() {
		Expect(sandbox.ConfigureIdentity(ctx, proxy, sandbox.Identity{
			Name:  "Kvarn Bot",
			Email: "bot@example.com",
		})).To(Succeed())

		Expect(proxy.execCalls).To(HaveLen(2))
		for _, req := range proxy.execCalls {
			Expect(req.Command).To(Equal("git"))
			// Root's config is not the one the workspace commands read.
			Expect(req.Privileged).To(BeFalse())
			Expect(req.Args[:2]).To(Equal([]string{"config", "--global"}))
		}
		Expect(proxy.execCalls[0].Args).To(Equal([]string{"config", "--global", "--", "user.name", "Kvarn Bot"}))
		Expect(proxy.execCalls[1].Args).To(Equal([]string{"config", "--global", "--", "user.email", "bot@example.com"}))
	})

	It("keeps a value that starts with a dash out of git's options", func() {
		Expect(sandbox.ConfigureIdentity(ctx, proxy, sandbox.Identity{
			Name:  "-f",
			Email: "dash@example.com",
		})).To(Succeed())

		Expect(proxy.execCalls[0].Args).To(Equal([]string{"config", "--global", "--", "user.name", "-f"}))
	})

	It("falls back to the default identity when the caller has none", func() {
		Expect(sandbox.ConfigureIdentity(ctx, proxy, sandbox.Identity{})).To(Succeed())

		Expect(proxy.execCalls[0].Args).To(ContainElement(sandbox.DefaultIdentity.Name))
		Expect(proxy.execCalls[1].Args).To(ContainElement(sandbox.DefaultIdentity.Email))
	})

	It("is spelled the way a real git accepts", func() {
		home := GinkgoT().TempDir()
		runner := &homedGitRunner{mockProxy: newMockProxy(), home: home}

		Expect(sandbox.ConfigureIdentity(ctx, runner, sandbox.Identity{
			Name:  "-f",
			Email: "bot@example.com",
		})).To(Succeed())

		Expect(gitGlobal(home, "user.name")).To(Equal("-f"))
		Expect(gitGlobal(home, "user.email")).To(Equal("bot@example.com"))
	})

	It("fails with the guest's stderr when git rejects the setting", func() {
		proxy.pushExecResponse(&v1.ExecResponse{
			ExitCode: 4,
			Stderr:   "fatal: not in a git directory",
		}, nil)

		err := sandbox.ConfigureIdentity(ctx, proxy, sandbox.Identity{Name: "a", Email: "b@c"})
		Expect(err).To(MatchError(ContainSubstring("not in a git directory")))
		Expect(err).To(MatchError(ContainSubstring("user.name")))
	})
})
