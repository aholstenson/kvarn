package sandbox

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("OwnedDirs", func() {
	It("returns the full chain below the kvarn home for a nested path", func() {
		Expect(OwnedDirs("/home/kvarn/.cache/nix")).To(Equal([]string{
			"/home/kvarn/.cache",
			"/home/kvarn/.cache/nix",
		}))
	})

	It("returns the deeper chain for multi-level paths", func() {
		Expect(OwnedDirs("/home/kvarn/.bun/install/cache")).To(Equal([]string{
			"/home/kvarn/.bun",
			"/home/kvarn/.bun/install",
			"/home/kvarn/.bun/install/cache",
		}))
	})

	It("returns just the leaf for a direct child of the home dir", func() {
		Expect(OwnedDirs("/home/kvarn/go")).To(Equal([]string{"/home/kvarn/go"}))
	})

	It("returns just the leaf for paths outside the home dir", func() {
		Expect(OwnedDirs("/opt/cache")).To(Equal([]string{"/opt/cache"}))
	})
})
