package preview_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/preview"
)

var _ = Describe("Auto-start routes", func() {
	const domain = "preview.example.com"

	compile := func(pattern string) preview.Route {
		GinkgoHelper()
		route, err := preview.CompileRoute("acme", pattern, domain)
		Expect(err).NotTo(HaveOccurred())
		return route
	}

	Describe("compiling a pattern", func() {
		It("rejects a pattern that does not end in the preview domain", func() {
			_, err := preview.CompileRoute("acme", "pr-{pr}.example.org", domain)
			Expect(err).To(MatchError(ContainSubstring("must end in .{domain}")))
		})

		It("rejects a pattern with no pull request in it", func() {
			_, err := preview.CompileRoute("acme", "staging.{domain}", domain)
			Expect(err).To(MatchError(ContainSubstring("must use {pr} exactly once")))
		})

		It("rejects a pattern that names the pull request twice", func() {
			_, err := preview.CompileRoute("acme", "pr-{pr}-{pr}.{domain}", domain)
			Expect(err).To(MatchError(ContainSubstring("must use {pr} exactly once")))
		})

		It("rejects a pull request that is a whole label, which would claim the zone", func() {
			_, err := preview.CompileRoute("acme", "{pr}.{domain}", domain)
			Expect(err).To(MatchError(ContainSubstring("literal prefix")))
		})

		It("rejects placeholders it cannot match, such as the ref", func() {
			// A ref label is slugged and digested on the way in, and nothing
			// recovers the ref from it.
			_, err := preview.CompileRoute("acme", "pr-{pr}-{ref}.{domain}", domain)
			Expect(err).To(MatchError(ContainSubstring("placeholder")))
		})

		It("rejects a pattern with a scheme or a path", func() {
			_, err := preview.CompileRoute("acme", "https://pr-{pr}.{domain}", domain)
			Expect(err).To(MatchError(ContainSubstring("without a scheme or a path")))
		})

		It("refuses to match without a domain to form names under", func() {
			_, err := preview.CompileRoute("acme", "pr-{pr}.{domain}", "")
			Expect(err).To(MatchError(ContainSubstring("no preview domain")))
		})
	})

	Describe("matching a hostname", func() {
		It("reads the pull request out of a name it claims", func() {
			pr, ok := compile("pr-{pr}.{domain}").Match("pr-42.preview.example.com")
			Expect(ok).To(BeTrue())
			Expect(pr).To(Equal("42"))
		})

		It("matches regardless of case, port or trailing dot", func() {
			pr, ok := compile("pr-{pr}.{domain}").Match("PR-42.Preview.Example.Com.:8080")
			Expect(ok).To(BeTrue())
			Expect(pr).To(Equal("42"))
		})

		It("does not match a name in a different zone", func() {
			_, ok := compile("pr-{pr}.{domain}").Match("pr-42.preview.example.org")
			Expect(ok).To(BeFalse())
		})

		It("does not match when the pull request part is empty", func() {
			_, ok := compile("pr-{pr}.{domain}").Match("pr-.preview.example.com")
			Expect(ok).To(BeFalse())
		})

		It("does not let the match cross a label boundary", func() {
			// Without this, `pr-1.evil` under the domain would be read as the
			// pull request `1.evil` and sent to the forge as one.
			_, ok := compile("pr-{pr}.{domain}").Match("pr-1.evil.preview.example.com")
			Expect(ok).To(BeFalse())
		})

		It("keeps text that follows the pull request in its label out of the match", func() {
			route := compile("pr-{pr}-app.{domain}")
			pr, ok := route.Match("pr-7-app.preview.example.com")
			Expect(ok).To(BeTrue())
			Expect(pr).To(Equal("7"))

			_, ok = route.Match("pr-7.preview.example.com")
			Expect(ok).To(BeFalse())
		})

		It("rejects a pull request with characters a hostname label cannot hold", func() {
			_, ok := compile("pr-{pr}.{domain}").Match("pr-4_2.preview.example.com")
			Expect(ok).To(BeFalse())
		})
	})

	Describe("the router", func() {
		It("routes each project's own family of names", func() {
			acme, err := preview.CompileRoute("acme", "pr-{pr}.{domain}", "acme.example.com")
			Expect(err).NotTo(HaveOccurred())
			other, err := preview.CompileRoute("other", "pr-{pr}.{domain}", "other.example.com")
			Expect(err).NotTo(HaveOccurred())

			router, err := preview.NewRouter([]preview.Route{acme, other})
			Expect(err).NotTo(HaveOccurred())

			match, err := router.Match("pr-3.other.example.com")
			Expect(err).NotTo(HaveOccurred())
			Expect(match.Project).To(Equal("other"))
			Expect(match.PR).To(Equal("3"))
		})

		It("refuses two projects claiming one family of names", func() {
			first, err := preview.CompileRoute("acme", "pr-{pr}.{domain}", domain)
			Expect(err).NotTo(HaveOccurred())
			second, err := preview.CompileRoute("other", "pr-{pr}.{domain}", domain)
			Expect(err).NotTo(HaveOccurred())

			_, err = preview.NewRouter([]preview.Route{first, second})
			Expect(err).To(MatchError(ContainSubstring("both claim")))
		})

		It("reports a name nothing claims", func() {
			router, err := preview.NewRouter([]preview.Route{compile("pr-{pr}.{domain}")})
			Expect(err).NotTo(HaveOccurred())

			_, err = router.Match("www.preview.example.com")
			Expect(err).To(MatchError(preview.ErrNoRoute))
		})

		It("claims nothing when it has no routes", func() {
			router, err := preview.NewRouter(nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(router.Empty()).To(BeTrue())
			_, err = router.Match("pr-1.preview.example.com")
			Expect(err).To(MatchError(preview.ErrNoRoute))
		})
	})
})
