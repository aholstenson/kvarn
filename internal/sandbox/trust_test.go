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

		Expect(proxy.execCalls).NotTo(BeEmpty())
		Expect(proxy.execCalls[0].Command).To(Equal("update-ca-certificates"))
		Expect(proxy.execCalls[0].Privileged).To(BeTrue())
	})

	It("adds the certificate to the job user's NSS database", func() {
		err := sandbox.InstallProxyCA(ctx, proxy, caPEM)
		Expect(err).NotTo(HaveOccurred())

		Expect(proxy.execCalls).To(HaveLen(2))
		cmd := proxy.execCalls[1].Command
		Expect(cmd).To(ContainSubstring("mkdir -p /home/kvarn/.pki/nssdb"))
		Expect(cmd).To(ContainSubstring(`certutil -d sql:/home/kvarn/.pki/nssdb -A -t "C,," ` +
			`-n kvarn-egress-proxy -i /usr/local/share/ca-certificates/kvarn-proxy.crt`))

		// Unprivileged so the database lands under the job user's own home
		// and ownership.
		Expect(proxy.execCalls[1].Privileged).To(BeFalse())
	})

	It("keeps the job running when the NSS database cannot be updated", func() {
		// Only browsers depend on that store, and the accepted image range
		// reaches back past certutil being installed.
		proxy.pushExecResponse(&v1.ExecResponse{ExitCode: 0}, nil)
		proxy.pushExecResponse(&v1.ExecResponse{
			ExitCode: 127,
			Stderr:   "certutil: not found\n",
		}, nil)

		Expect(sandbox.InstallProxyCA(ctx, proxy, caPEM)).To(Succeed())
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
