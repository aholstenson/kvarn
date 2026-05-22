package orchestrator

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("branchSlug", func() {
	DescribeTable("derives a git-ref-safe slug from a commit title",
		func(title, expected string) {
			Expect(branchSlug(title)).To(Equal(expected))
		},
		Entry("strips a plain Conventional Commit type",
			"feat: Add authentication support", "add-authentication-support"),
		Entry("strips a scoped Conventional Commit prefix",
			"chore(main): Release 0.1.0", "release-0-1-0"),
		Entry("strips a breaking-change marker",
			"feat!: Drop legacy API", "drop-legacy-api"),
		Entry("strips a scoped breaking-change marker",
			"refactor(api)!: Rename handler", "rename-handler"),
		Entry("slugifies a title without a Conventional Commit prefix",
			"Just some free-form title", "just-some-free-form-title"),
		Entry("collapses punctuation and slashes into single hyphens",
			"fix: handle nil/empty   values!!", "handle-nil-empty-values"),
		Entry("lowercases mixed case",
			"feat: Add HTTP Server", "add-http-server"),
		Entry("does not treat a non-type word followed by colon as a prefix",
			"Note: this is fine", "note-this-is-fine"),
	)

	It("returns an empty slug when nothing usable remains", func() {
		Expect(branchSlug("feat:")).To(Equal(""))
		Expect(branchSlug("!!!")).To(Equal(""))
		Expect(branchSlug("")).To(Equal(""))
	})

	It("truncates long slugs at a word boundary", func() {
		slug := branchSlug("refactor: Use built-in errors instead of cockroachdb errors package")
		Expect(len(slug)).To(BeNumerically("<=", maxBranchSlugLen))
		// No leading or trailing hyphens after truncation.
		Expect(slug).NotTo(HavePrefix("-"))
		Expect(slug).NotTo(HaveSuffix("-"))
		Expect(slug).To(Equal("use-built-in-errors-instead-of-cockroachdb-errors"))
	})

	It("hard-truncates a single long word with no boundary", func() {
		long := "feat: " + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		slug := branchSlug(long)
		Expect(len(slug)).To(Equal(maxBranchSlugLen))
	})
})
