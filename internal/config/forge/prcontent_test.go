package forge_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
)

func ptrInt(v int) *int    { return &v }
func ptrBool(v bool) *bool { return &v }

var _ = Describe("ResolveBehavior pull-request content", func() {
	It("falls back to the compiled-in content defaults", func() {
		c := (&forgeconfig.ForgeConfig{}).ResolveBehavior(
			forgeconfig.Defaults{}, forgeconfig.Overrides{}, forgeconfig.RepoContent{}).PullRequest

		Expect(c.TitleMaxLength).To(Equal(forgeconfig.DefaultTitleMaxLength))
		Expect(c.ReportWorklog).To(BeTrue())
		Expect(c.ReportCost).To(BeTrue())
		Expect(c.TitleInstructions.Empty()).To(BeTrue())
		Expect(c.BodySections).To(BeEmpty())
	})

	Describe("instructions", func() {
		It("concatenates every operator layer into the organization half", func() {
			c := (&forgeconfig.ForgeConfig{
				PullRequest: forgeconfig.PRContent{BodyInstructions: "from the forge"},
			}).ResolveBehavior(
				forgeconfig.Defaults{PullRequest: forgeconfig.PRContent{BodyInstructions: "from the defaults"}},
				forgeconfig.Overrides{PullRequest: forgeconfig.PRContent{BodyInstructions: "from the project"}},
				forgeconfig.RepoContent{},
			).PullRequest

			Expect(c.BodyInstructions.Organization).To(Equal(
				"from the defaults\n\nfrom the forge\n\nfrom the project"),
				"least specific first, so the most specific reads last")
			Expect(c.BodyInstructions.Repository).To(BeEmpty())
		})

		It("keeps the repository's own instructions separate rather than merging them", func() {
			c := (&forgeconfig.ForgeConfig{}).ResolveBehavior(
				forgeconfig.Defaults{PullRequest: forgeconfig.PRContent{TitleInstructions: "Conventional Commits."}},
				forgeconfig.Overrides{},
				forgeconfig.RepoContent{TitleInstructions: "Scope by package."},
			).PullRequest

			Expect(c.TitleInstructions.Organization).To(Equal("Conventional Commits."))
			Expect(c.TitleInstructions.Repository).To(Equal("Scope by package."))
			Expect(c.TitleInstructions.Empty()).To(BeFalse())
		})

		It("skips layers that set nothing", func() {
			c := (&forgeconfig.ForgeConfig{}).ResolveBehavior(
				forgeconfig.Defaults{PullRequest: forgeconfig.PRContent{CommentInstructions: "  "}},
				forgeconfig.Overrides{PullRequest: forgeconfig.PRContent{CommentInstructions: "Be terse."}},
				forgeconfig.RepoContent{},
			).PullRequest

			Expect(c.CommentInstructions.Organization).To(Equal("Be terse."),
				"a whitespace-only layer must not leave a blank line behind")
		})
	})

	Describe("scalars", func() {
		It("lets the most specific operator layer win", func() {
			c := (&forgeconfig.ForgeConfig{
				PullRequest: forgeconfig.PRContent{BodyFooter: "forge footer", TitleMaxLength: ptrInt(60)},
			}).ResolveBehavior(
				forgeconfig.Defaults{PullRequest: forgeconfig.PRContent{BodyFooter: "default footer"}},
				forgeconfig.Overrides{PullRequest: forgeconfig.PRContent{BodyFooter: "project footer"}},
				forgeconfig.RepoContent{},
			).PullRequest

			Expect(c.BodyFooter).To(Equal("project footer"))
			Expect(c.TitleMaxLength).To(Equal(60))
		})

		It("lets the repository set the title budget above every operator layer", func() {
			c := (&forgeconfig.ForgeConfig{}).ResolveBehavior(
				forgeconfig.Defaults{PullRequest: forgeconfig.PRContent{TitleMaxLength: ptrInt(50)}},
				forgeconfig.Overrides{PullRequest: forgeconfig.PRContent{TitleMaxLength: ptrInt(60)}},
				forgeconfig.RepoContent{TitleMaxLength: ptrInt(80)},
			).PullRequest

			Expect(c.TitleMaxLength).To(Equal(80))
		})

		It("replaces trailers wholesale rather than merging them", func() {
			c := (&forgeconfig.ForgeConfig{}).ResolveBehavior(
				forgeconfig.Defaults{PullRequest: forgeconfig.PRContent{
					CommitTrailers: []string{"Default-Trailer: a", "Second: b"},
				}},
				forgeconfig.Overrides{PullRequest: forgeconfig.PRContent{
					CommitTrailers: []string{"Project-Trailer: c"},
				}},
				forgeconfig.RepoContent{},
			).PullRequest

			Expect(c.CommitTrailers).To(Equal([]string{"Project-Trailer: c"}))
		})

		It("resolves the report toggles independently", func() {
			c := (&forgeconfig.ForgeConfig{}).ResolveBehavior(
				forgeconfig.Defaults{PullRequest: forgeconfig.PRContent{ReportWorklog: ptrBool(false)}},
				forgeconfig.Overrides{},
				forgeconfig.RepoContent{},
			).PullRequest

			Expect(c.ReportWorklog).To(BeFalse())
			Expect(c.ReportCost).To(BeTrue())
		})

		It("lets the most specific layer set the quote mode", func() {
			c := (&forgeconfig.ForgeConfig{
				PullRequest: forgeconfig.PRContent{QuoteTask: forgeconfig.QuoteCollapsed},
			}).ResolveBehavior(
				forgeconfig.Defaults{PullRequest: forgeconfig.PRContent{QuoteTask: forgeconfig.QuoteOff}},
				forgeconfig.Overrides{},
				forgeconfig.RepoContent{},
			).PullRequest

			Expect(c.QuoteTask).To(Equal(forgeconfig.QuoteCollapsed))
		})

		It("defaults the quote mode to auto when no layer sets one", func() {
			c := (&forgeconfig.ForgeConfig{}).ResolveBehavior(
				forgeconfig.Defaults{}, forgeconfig.Overrides{}, forgeconfig.RepoContent{}).PullRequest

			Expect(c.QuoteTask).To(Equal(forgeconfig.QuoteAuto))
		})
	})

	Describe("QuoteMode", func() {
		It("parses every documented spelling", func() {
			for _, want := range []forgeconfig.QuoteMode{
				forgeconfig.QuoteAuto, forgeconfig.QuoteCollapsed,
				forgeconfig.QuoteFull, forgeconfig.QuoteOff,
			} {
				var m forgeconfig.QuoteMode
				Expect(m.UnmarshalText([]byte(want))).To(Succeed())
				Expect(m).To(Equal(want))
			}
		})

		It("rejects an unknown spelling so a typo fails the load", func() {
			var m forgeconfig.QuoteMode
			err := m.UnmarshalText([]byte("colapsed"))
			Expect(err).To(MatchError(ContainSubstring("unknown quote mode")))
			Expect(err).To(MatchError(ContainSubstring("collapsed")))
		})
	})

	Describe("sections", func() {
		It("takes them from the repository, the only layer that can declare them", func() {
			c := (&forgeconfig.ForgeConfig{}).ResolveBehavior(
				forgeconfig.Defaults{}, forgeconfig.Overrides{},
				forgeconfig.RepoContent{
					BodySections: []forgeconfig.Section{
						{Name: "Testing", Description: "What was run.", Required: true},
					},
					CommentSections: []forgeconfig.Section{{Name: "Verdict"}},
				},
			).PullRequest

			Expect(c.BodySections).To(HaveLen(1))
			Expect(c.BodySections[0].Name).To(Equal("Testing"))
			Expect(c.BodySections[0].Required).To(BeTrue())
			Expect(c.CommentSections).To(HaveLen(1))
		})

		It("copies them so a resolved value does not alias the config it came from", func() {
			declared := []forgeconfig.Section{{Name: "Testing"}}
			c := (&forgeconfig.ForgeConfig{}).ResolveBehavior(
				forgeconfig.Defaults{}, forgeconfig.Overrides{},
				forgeconfig.RepoContent{BodySections: declared},
			).PullRequest

			declared[0].Name = "mutated"
			Expect(c.BodySections[0].Name).To(Equal("Testing"))
		})
	})

	It("skips the forge layer entirely for a project without one", func() {
		c := (*forgeconfig.ForgeConfig)(nil).ResolveBehavior(
			forgeconfig.Defaults{PullRequest: forgeconfig.PRContent{BodyInstructions: "house style"}},
			forgeconfig.Overrides{},
			forgeconfig.RepoContent{BodyInstructions: "repo style"},
		).PullRequest

		Expect(c.BodyInstructions.Organization).To(Equal("house style"))
		Expect(c.BodyInstructions.Repository).To(Equal("repo style"))
	})
})
