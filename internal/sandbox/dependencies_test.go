package sandbox_test

import (
	"context"

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
				FlakeURI: "github:NixOS/nixpkgs/nixos-25.11",
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
		Expect(req.Args[4]).To(ContainSubstring("github:NixOS/nixpkgs/nixos-25.11#hello"))
		Expect(req.Args[5:]).To(Equal([]string{"--", "kvarn"}))
	})

	It("merges multiple sources into one nix profile install", func() {
		deps := []project.ResolvedDep{
			{FlakeURI: "github:NixOS/nixpkgs/nixos-25.11", Attr: "nodejs", Host: "github.com"},
			{FlakeURI: "github:NixOS/nixpkgs/nixos-25.11", Attr: "go", Host: "github.com"},
			{FlakeURI: "github:NixOS/nixpkgs/nixos-unstable", Attr: "bun", Host: "github.com"},
		}
		err := sandbox.InstallDependencies(ctx, proxy, deps, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(proxy.execCalls).To(HaveLen(1))

		cmd := proxy.execCalls[0].Args[4]
		Expect(cmd).To(ContainSubstring("github:NixOS/nixpkgs/nixos-25.11#nodejs"))
		Expect(cmd).To(ContainSubstring("github:NixOS/nixpkgs/nixos-25.11#go"))
		Expect(cmd).To(ContainSubstring("github:NixOS/nixpkgs/nixos-unstable#bun"))
	})

	It("forwards stdout/stderr through the output callback", func() {
		proxy.pushExecResponse(&v1.ExecResponse{
			ExitCode: 0,
			Stdout:   "installed hello",
			Stderr:   "warning: foo",
		}, nil)
		deps := []project.ResolvedDep{
			{FlakeURI: "github:NixOS/nixpkgs/nixos-25.11", Attr: "hello", Host: "github.com"},
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
