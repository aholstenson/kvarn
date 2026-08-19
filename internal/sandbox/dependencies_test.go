package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"time"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("InstallDependencies", func() {
	var (
		ctx   context.Context
		proxy *mockProxy
	)

	BeforeEach(func() {
		ctx = context.Background()
		proxy = newMockProxy()
	})

	It("makes no exec calls when there are no dependencies", func() {
		err := sandbox.InstallDependencies(ctx, proxy, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(proxy.execCalls).To(BeEmpty())
	})

	It("issues a single nix profile install for one attribute", func() {
		deps := []project.ResolvedDep{
			{
				FlakeURI: project.DefaultNixpkgsFlake,
				Attr:     "hello",
				Host:     "github.com",
			},
		}
		err := sandbox.InstallDependencies(ctx, proxy, deps, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(proxy.execCalls).To(HaveLen(1))

		// `su -l -s /bin/sh -c '<nix command>' -- kvarn`.
		req := proxy.execCalls[0]
		Expect(req.Command).To(Equal("su"))
		Expect(req.Privileged).To(BeTrue())
		Expect(req.Args).To(HaveLen(7))
		Expect(req.Args[:4]).To(Equal([]string{"-l", "-s", "/bin/sh", "-c"}))
		Expect(req.Args[4]).To(ContainSubstring("nix profile add"))
		Expect(req.Args[4]).To(ContainSubstring(project.DefaultNixpkgsFlake + "#hello"))
		Expect(req.Args[5:]).To(Equal([]string{"--", "kvarn"}))
	})

	It("merges multiple sources into one nix profile install", func() {
		deps := []project.ResolvedDep{
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "nodejs", Host: "github.com"},
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "go", Host: "github.com"},
			{FlakeURI: "github:NixOS/nixpkgs/nixos-unstable", Attr: "bun", Host: "github.com"},
		}
		err := sandbox.InstallDependencies(ctx, proxy, deps, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(proxy.execCalls).To(HaveLen(1))

		cmd := proxy.execCalls[0].Args[4]
		Expect(cmd).To(ContainSubstring(project.DefaultNixpkgsFlake + "#nodejs"))
		Expect(cmd).To(ContainSubstring(project.DefaultNixpkgsFlake + "#go"))
		Expect(cmd).To(ContainSubstring("github:NixOS/nixpkgs/nixos-unstable#bun"))
	})

	It("caps a single install below the total budget", func() {
		deps := []project.ResolvedDep{
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "hello", Host: "github.com"},
		}
		Expect(sandbox.InstallDependencies(ctx, proxy, deps, nil)).To(Succeed())
		Expect(proxy.execCalls[0].TimeoutSeconds).To(BeNumerically("==", 30*60))
	})

	It("forwards stdout/stderr through the output callback", func() {
		proxy.pushExecResponse(&v1.ExecResponse{
			ExitCode: 0,
			Stdout:   "installed hello",
			Stderr:   "warning: foo",
		}, nil)
		deps := []project.ResolvedDep{
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "hello", Host: "github.com"},
		}

		var gotStdout, gotStderr string
		err := sandbox.InstallDependencies(ctx, proxy, deps, func(stdout, stderr string) {
			gotStdout = stdout
			gotStderr = stderr
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(gotStdout).To(Equal("installed hello"))
		Expect(gotStderr).To(Equal("warning: foo"))
	})
})

var _ = Describe("pinning nixpkgs channels", func() {
	const rev = "1111111111111111111111111111111111111111"

	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	It("installs from the commit the resolver reports", func() {
		proxy := newMockProxy()
		cfg := &project.Config{Dependencies: project.Dependencies{"nixpkgs": {"hello"}}}
		deps, err := cfg.Dependencies.Resolve()
		Expect(err).NotTo(HaveOccurred())

		deps = sandbox.PinNixpkgsRevsForTest(ctx, deps, func(context.Context, string) string { return rev })
		Expect(deps[0].FlakeURI).To(Equal(project.NixpkgsFlakePrefix + rev))

		// The channel is untouched, so the cache keys derived from it do not
		// move when the channel does.
		Expect(deps[0].Channel).To(Equal(project.DefaultNixpkgsChannel))
		Expect(deps[0].StableURI()).To(Equal(project.NixpkgsFlakePrefix + project.DefaultNixpkgsChannel))

		Expect(sandbox.InstallDependencies(ctx, proxy, deps, nil)).To(Succeed())
		Expect(proxy.execCalls[0].Args[4]).To(ContainSubstring(project.NixpkgsFlakePrefix + rev + "#hello"))
	})

	It("keeps the flake reference when the channel cannot be resolved", func() {
		deps := []project.ResolvedDep{
			{FlakeURI: "github:NixOS/nixpkgs/nixos-unstable", Attr: "bun", Channel: "nixos-unstable"},
		}
		deps = sandbox.PinNixpkgsRevsForTest(ctx, deps, func(context.Context, string) string { return "" })
		Expect(deps[0].FlakeURI).To(Equal("github:NixOS/nixpkgs/nixos-unstable"))
	})

	It("leaves sources that are not nixpkgs alone", func() {
		deps := []project.ResolvedDep{{FlakeURI: "github:owner/repo", Attr: "tool"}}
		deps = sandbox.PinNixpkgsRevsForTest(ctx, deps, func(context.Context, string) string { return rev })
		Expect(deps[0].FlakeURI).To(Equal("github:owner/repo"))
	})

	It("resolves each channel once however many attributes come from it", func() {
		deps := []project.ResolvedDep{
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "go", Channel: "nixos-26.05"},
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "nodejs", Channel: "nixos-26.05"},
			{FlakeURI: "github:NixOS/nixpkgs/nixos-unstable", Attr: "bun", Channel: "nixos-unstable"},
		}
		var channels []string
		sandbox.PinNixpkgsRevsForTest(ctx, deps, func(_ context.Context, channel string) string {
			channels = append(channels, channel)
			return rev
		})
		Expect(channels).To(ConsistOf("nixos-26.05", "nixos-unstable"))
	})
})

var _ = Describe("InstallDependencies retries", func() {
	// A GitHub API outage is what a cold install meets first, and it is the
	// shape Nix reports it in: its own attempts as warnings, then the error it
	// gave up on.
	const gatewayTimeout = `warning: unable to download 'https://api.github.com/repos/NixOS/nixpkgs/commits/nixos-26.05': HTTP error 504; retrying in 298 ms
error: unable to download 'https://api.github.com/repos/NixOS/nixpkgs/commits/nixos-26.05': HTTP error 504`

	// An attribute that does not exist fails the same way on every attempt.
	const missingAttr = `warning: unable to download 'https://cache.nixos.org/x.narinfo': HTTP error 504; retrying in 298 ms
error: flake 'github:NixOS/nixpkgs' does not provide attribute 'packages.x86_64-linux.nodejs99'`

	var (
		ctx     context.Context
		proxy   *mockProxy
		deps    []project.ResolvedDep
		restore func()
	)

	BeforeEach(func() {
		ctx = context.Background()
		proxy = newMockProxy()
		deps = []project.ResolvedDep{
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "hello", Host: "github.com"},
		}
		restore = sandbox.SetDependencyRetryBackoffForTest(
			[]time.Duration{time.Millisecond, time.Millisecond})
	})

	AfterEach(func() { restore() })

	It("retries a network failure and succeeds on a later attempt", func() {
		proxy.pushExecResponse(&v1.ExecResponse{ExitCode: 1, Stderr: gatewayTimeout}, nil)
		proxy.pushExecResponse(&v1.ExecResponse{ExitCode: 0, Stdout: "installed hello"}, nil)

		Expect(sandbox.InstallDependencies(ctx, proxy, deps, nil)).To(Succeed())
		Expect(proxy.execCalls).To(HaveLen(2))
	})

	It("gives up once the retries are spent", func() {
		for range 3 {
			proxy.pushExecResponse(&v1.ExecResponse{ExitCode: 1, Stderr: gatewayTimeout}, nil)
		}

		err := sandbox.InstallDependencies(ctx, proxy, deps, nil)
		Expect(err).To(MatchError(ContainSubstring("after 3 attempts")))
		Expect(err).To(MatchError(ContainSubstring("HTTP error 504")))
		Expect(proxy.execCalls).To(HaveLen(3))
	})

	It("reports a failure that is not transient without retrying", func() {
		proxy.pushExecResponse(&v1.ExecResponse{ExitCode: 1, Stderr: missingAttr}, nil)

		err := sandbox.InstallDependencies(ctx, proxy, deps, nil)
		Expect(err).To(MatchError(ContainSubstring("does not provide attribute")))
		Expect(err).To(MatchError(ContainSubstring("after 1 attempt")))
		Expect(proxy.execCalls).To(HaveLen(1))
	})

	It("drops the warning noise from the reported failure", func() {
		proxy.pushExecResponse(&v1.ExecResponse{ExitCode: 1, Stderr: missingAttr}, nil)

		err := sandbox.InstallDependencies(ctx, proxy, deps, nil)
		Expect(err.Error()).NotTo(ContainSubstring("retrying in 298 ms"))
	})

	It("tells the caller a retry is coming", func() {
		proxy.pushExecResponse(&v1.ExecResponse{ExitCode: 1, Stderr: gatewayTimeout}, nil)
		proxy.pushExecResponse(&v1.ExecResponse{ExitCode: 0}, nil)

		var streamed []string
		Expect(sandbox.InstallDependencies(ctx, proxy, deps, func(_, stderr string) {
			streamed = append(streamed, stderr)
		})).To(Succeed())
		Expect(strings.Join(streamed, "")).To(ContainSubstring("retrying in 1ms (attempt 2 of 3)"))
	})

	It("does not retry when the command could not be run at all", func() {
		proxy.pushExecResponse(nil, errors.New("bridge closed"))

		err := sandbox.InstallDependencies(ctx, proxy, deps, nil)
		Expect(err).To(MatchError(ContainSubstring("bridge closed")))
		Expect(proxy.execCalls).To(HaveLen(1))
	})

	It("stops retrying when the context is cancelled", func() {
		cancelCtx, cancel := context.WithCancel(ctx)
		proxy.pushExecResponse(&v1.ExecResponse{ExitCode: 1, Stderr: gatewayTimeout}, nil)
		cancel()

		err := sandbox.InstallDependencies(cancelCtx, proxy, deps, nil)
		Expect(err).To(MatchError(context.Canceled))
		Expect(proxy.execCalls).To(HaveLen(1))
	})
})
