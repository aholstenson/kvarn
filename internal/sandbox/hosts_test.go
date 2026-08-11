package sandbox_test

import (
	"context"
	"errors"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ConfigureHostAliases", func() {
	var (
		ctx     context.Context
		proxy   *mockProxy
		aliases map[string]string
	)

	BeforeEach(func() {
		ctx = context.Background()
		proxy = newMockProxy()
		aliases = map[string]string{
			"dev-shop.example.local":  "127.0.0.1",
			"dev-admin.example.local": "127.0.0.1",
		}
	})

	It("stages the entries as a marked block ordered by name", func() {
		Expect(sandbox.ConfigureHostAliases(ctx, proxy, aliases)).To(Succeed())

		Expect(proxy.uploadCalls).To(HaveLen(1))
		req := proxy.uploadCalls[0]
		Expect(req.WorkingDir).To(Equal("/run"))
		Expect(req.Files).To(HaveLen(1))
		Expect(req.Files[0].Path).To(Equal("kvarn-hosts"))
		Expect(string(req.Files[0].Content)).To(Equal(
			"\n# kvarn: network.host_aliases from kvarn.yml\n" +
				"127.0.0.1\tdev-admin.example.local\n" +
				"127.0.0.1\tdev-shop.example.local\n"))
	})

	It("appends to /etc/hosts as root rather than replacing it", func() {
		Expect(sandbox.ConfigureHostAliases(ctx, proxy, aliases)).To(Succeed())

		Expect(proxy.execCalls).To(HaveLen(1))
		call := proxy.execCalls[0]
		Expect(call.Privileged).To(BeTrue())
		Expect(call.Command).To(Equal("cat /run/kvarn-hosts >> /etc/hosts && rm -f /run/kvarn-hosts"))
	})

	It("does nothing when no aliases are configured", func() {
		Expect(sandbox.ConfigureHostAliases(ctx, proxy, nil)).To(Succeed())
		Expect(proxy.uploadCalls).To(BeEmpty())
		Expect(proxy.execCalls).To(BeEmpty())
	})

	It("fails when the block cannot be staged", func() {
		proxy.uploadError = errors.New("read-only filesystem")

		err := sandbox.ConfigureHostAliases(ctx, proxy, aliases)
		Expect(err).To(MatchError(ContainSubstring("read-only filesystem")))
		Expect(proxy.execCalls).To(BeEmpty())
	})

	It("fails when the append exits non-zero", func() {
		proxy.pushExecResponse(&v1.ExecResponse{
			ExitCode: 1,
			Stderr:   "/etc/hosts: Permission denied\n",
		}, nil)

		err := sandbox.ConfigureHostAliases(ctx, proxy, aliases)
		Expect(err).To(MatchError(ContainSubstring("exit 1")))
		Expect(err).To(MatchError(ContainSubstring("Permission denied")))
	})
})
