//go:build embedrunner

package runnerbin_test

import (
	"runtime"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/runnerbin"
)

var _ = Describe("Bytes", func() {
	It("returns the embedded runner for the build arch", func() {
		b, err := runnerbin.Bytes(runtime.GOARCH)
		Expect(err).NotTo(HaveOccurred())
		Expect(b).NotTo(BeEmpty())
	})

	It("rejects a mismatched arch", func() {
		_, err := runnerbin.Bytes("does-not-exist")
		Expect(err).To(HaveOccurred())
	})
})
