package sandbox_test

import (
	"context"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("CheckoutWorktree", func() {
	var (
		ctx   context.Context
		proxy *mockProxy
	)

	BeforeEach(func() {
		ctx = context.Background()
		proxy = newMockProxy()
	})

	It("checks out HEAD in the workspace as the unprivileged user", func() {
		Expect(sandbox.CheckoutWorktree(ctx, proxy, "/home/kvarn/workspace")).To(Succeed())

		Expect(proxy.execCalls).To(HaveLen(1))
		req := proxy.execCalls[0]
		Expect(req.Command).To(Equal("git"))
		Expect(req.WorkingDir).To(Equal("/home/kvarn/workspace"))
		// Privileged would run the checkout as root and leave a workspace the
		// agent cannot write to.
		Expect(req.Privileged).To(BeFalse())
		Expect(req.Args[len(req.Args)-3:]).To(Equal([]string{"reset", "--hard", "HEAD"}))
	})

	It("neutralizes the LFS filter", func() {
		Expect(sandbox.CheckoutWorktree(ctx, proxy, "/home/kvarn/workspace")).To(Succeed())

		// Running the real smudge filter would replace pointer files with
		// their contents and reach for an LFS endpoint from inside the VM,
		// which is the one thing the sandbox must never be able to do.
		Expect(proxy.execCalls[0].Args).To(ContainElements(
			"filter.lfs.smudge=cat",
			"filter.lfs.required=false",
		))
	})

	It("fails with the guest's stderr when the checkout fails", func() {
		proxy.pushExecResponse(&v1.ExecResponse{
			ExitCode: 128,
			Stderr:   "fatal: not a git repository",
		}, nil)

		err := sandbox.CheckoutWorktree(ctx, proxy, "/home/kvarn/workspace")
		Expect(err).To(MatchError(ContainSubstring("not a git repository")))
		Expect(err).To(MatchError(ContainSubstring("128")))
	})
})
