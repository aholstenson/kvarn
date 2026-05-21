package cache_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/sandbox/cache"
)

var _ = Describe("ProjectID", func() {
	It("returns a 16-character hex string", func() {
		id := cache.ProjectID("/home/user/my-project")
		Expect(id).To(HaveLen(16))
		Expect(id).To(MatchRegexp("^[0-9a-f]{16}$"))
	})

	It("is deterministic", func() {
		a := cache.ProjectID("/home/user/my-project")
		b := cache.ProjectID("/home/user/my-project")
		Expect(a).To(Equal(b))
	})

	It("produces different IDs for different paths", func() {
		a := cache.ProjectID("/home/user/project-a")
		b := cache.ProjectID("/home/user/project-b")
		Expect(a).NotTo(Equal(b))
	})
})
