package sandbox_test

import (
	"context"
	"errors"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("InstallProxyCA", func() {
	var (
		ctx   context.Context
		proxy *mockProxy
		caPEM []byte
	)

	BeforeEach(func() {
		ctx = context.Background()
		proxy = newMockProxy()
		caPEM = []byte("-----BEGIN CERTIFICATE-----\nkvarn\n-----END CERTIFICATE-----\n")
	})

	It("writes the certificate into the trust anchor directory", func() {
		err := sandbox.InstallProxyCA(ctx, proxy, caPEM)
		Expect(err).NotTo(HaveOccurred())

		Expect(proxy.uploadCalls).To(HaveLen(1))
		req := proxy.uploadCalls[0]
		Expect(req.WorkingDir).To(Equal("/usr/local/share/ca-certificates"))
		Expect(req.Files).To(HaveLen(1))
		Expect(req.Files[0].Path).To(Equal("kvarn-proxy.crt"))
		Expect(req.Files[0].Content).To(Equal(caPEM))
		Expect(req.Files[0].Mode).To(Equal(uint32(0o644)))
	})

	It("rebuilds the combined bundle as root", func() {
		err := sandbox.InstallProxyCA(ctx, proxy, caPEM)
		Expect(err).NotTo(HaveOccurred())

		Expect(proxy.execCalls).To(HaveLen(1))
		Expect(proxy.execCalls[0].Command).To(Equal("update-ca-certificates"))
		Expect(proxy.execCalls[0].Privileged).To(BeTrue())
	})

	It("does nothing when the provider supplies no CA", func() {
		err := sandbox.InstallProxyCA(ctx, proxy, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(proxy.uploadCalls).To(BeEmpty())
		Expect(proxy.execCalls).To(BeEmpty())
	})

	It("fails when the certificate cannot be written", func() {
		proxy.uploadError = errors.New("read-only filesystem")

		err := sandbox.InstallProxyCA(ctx, proxy, caPEM)
		Expect(err).To(MatchError(ContainSubstring("read-only filesystem")))
		Expect(proxy.execCalls).To(BeEmpty())
	})

	It("fails when update-ca-certificates exits non-zero", func() {
		proxy.pushExecResponse(&v1.ExecResponse{
			ExitCode: 1,
			Stderr:   "unable to parse certificate\n",
		}, nil)

		err := sandbox.InstallProxyCA(ctx, proxy, caPEM)
		Expect(err).To(MatchError(ContainSubstring("exit 1")))
		Expect(err).To(MatchError(ContainSubstring("unable to parse certificate")))
	})
})
