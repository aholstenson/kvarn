package coding

import (
	"reflect"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
)

// The summary helpers are unexported, so this suite is an internal test.

var _ = Describe("summarySystemPrompt", func() {
	It("labels instructions by origin so a conflict is legible", func() {
		out := summarySystemPrompt(forgeconfig.Content{
			TitleInstructions: forgeconfig.Instructions{Organization: "Conventional Commits."},
			BodyInstructions:  forgeconfig.Instructions{Repository: "Mention flag changes."},
		}, "")

		Expect(out).To(ContainSubstring("## Organization conventions"))
		Expect(out).To(ContainSubstring("Conventional Commits."))
		Expect(out).To(ContainSubstring("## Repository conventions"))
		Expect(out).To(ContainSubstring("Mention flag changes."))
		Expect(strings.Index(out, "## Organization conventions")).To(
			BeNumerically("<", strings.Index(out, "## Repository conventions")),
			"the most specific conventions read last")
	})

	It("omits a heading nothing populated", func() {
		out := summarySystemPrompt(forgeconfig.Content{}, "")
		Expect(out).NotTo(ContainSubstring("## Organization conventions"))
		Expect(out).NotTo(ContainSubstring("## Repository conventions"))
	})

	It("still carries the repository instruction file", func() {
		out := summarySystemPrompt(forgeconfig.Content{}, "House rules from CLAUDE.md.")
		Expect(out).To(ContainSubstring("House rules from CLAUDE.md."))
	})
})

var _ = Describe("summaryPrompt", func() {
	It("states the configured title budget", func() {
		Expect(summaryPrompt(forgeconfig.Content{TitleMaxLength: 50}, nil)).To(
			ContainSubstring("max 50 chars"))
	})

	It("lists requested sections and marks the required ones", func() {
		out := summaryPrompt(forgeconfig.Content{
			TitleMaxLength: 72,
			BodySections: []forgeconfig.Section{
				{Name: "Testing", Description: "What ran.", Required: true},
				{Name: "Risks"},
			},
		}, nil)

		Expect(out).To(ContainSubstring("- Testing (required): What ran."))
		Expect(out).To(ContainSubstring("- Risks\n"))
	})

	It("tells the model to produce no sections when none are declared", func() {
		out := summaryPrompt(forgeconfig.Content{TitleMaxLength: 72}, nil)
		Expect(out).To(ContainSubstring("leave empty; none are requested"))
	})

	It("passes recent commit subjects through for style matching", func() {
		out := summaryPrompt(forgeconfig.Content{TitleMaxLength: 72}, []string{"feat: add retries"})
		Expect(out).To(ContainSubstring("feat: add retries"))
	})

	It("asks for the description before the title, as the schema orders them", func() {
		out := summaryPrompt(forgeconfig.Content{TitleMaxLength: 72}, nil)
		Expect(strings.Index(out, "- description:")).To(
			BeNumerically("<", strings.Index(out, "- title:")))
	})
})

var _ = Describe("AgentSummary", func() {
	It("declares the title last so it is generated after the body", func() {
		t := reflect.TypeOf(AgentSummary{})
		var title, last int
		for i := range t.NumField() {
			if t.Field(i).Name == "Title" {
				title = i
			}
			last = i
		}
		Expect(title).To(Equal(last),
			"constrained generation fills schema properties in declaration order and cannot revise them")
	})
})

var _ = Describe("summaryProblems", func() {
	content := forgeconfig.Content{
		TitleMaxLength: 20,
		BodySections: []forgeconfig.Section{
			{Name: "Testing", Required: true},
			{Name: "Risks"},
		},
	}

	It("accepts a summary that meets everything asked of it", func() {
		Expect(summaryProblems(AgentSummary{
			Title:    "fix: short enough",
			Sections: []SummarySection{{Name: "Testing", Content: "go test ./..."}},
		}, content)).To(BeEmpty())
	})

	It("reports a title over the budget", func() {
		problems := summaryProblems(AgentSummary{
			Title:    "fix: this subject line is far too long to fit",
			Sections: []SummarySection{{Name: "Testing", Content: "ran"}},
		}, content)
		Expect(problems).To(HaveLen(1))
		Expect(problems[0]).To(ContainSubstring("it must be at most 20"))
	})

	It("counts characters rather than bytes", func() {
		Expect(summaryProblems(AgentSummary{
			Title:    "fix: åäöåäöåäöåäö",
			Sections: []SummarySection{{Name: "Testing", Content: "ran"}},
		}, content)).To(BeEmpty(), "16 runes fits a 20-character budget")
	})

	It("reports a required section that is missing or blank", func() {
		Expect(summaryProblems(AgentSummary{Title: "ok"}, content)).To(
			ContainElement(ContainSubstring(`required section "Testing" is missing`)))

		Expect(summaryProblems(AgentSummary{
			Title:    "ok",
			Sections: []SummarySection{{Name: "Testing", Content: "   "}},
		}, content)).To(HaveLen(1))
	})

	It("does not mind an optional section being left out", func() {
		Expect(summaryProblems(AgentSummary{
			Title:    "ok",
			Sections: []SummarySection{{Name: "Testing", Content: "ran"}},
		}, content)).To(BeEmpty())
	})

	It("reports an empty title", func() {
		Expect(summaryProblems(AgentSummary{
			Title:    "   ",
			Sections: []SummarySection{{Name: "Testing", Content: "ran"}},
		}, content)).To(ContainElement(ContainSubstring("the title is empty")))
	})

	DescribeTable("reports a filler title",
		func(title string) {
			Expect(summaryProblems(AgentSummary{
				Title:    title,
				Sections: []SummarySection{{Name: "Testing", Content: "ran"}},
			}, content)).To(ContainElement(ContainSubstring("is a placeholder")))
		},
		Entry("the bare word", "placeholder"),
		Entry("cased", "Placeholder"),
		Entry("trailing json punctuation from an abandoned answer", "placeholder}{"),
		Entry("angle-bracketed", "<TODO>"),
		Entry("naming the field instead of filling it", "Commit message"),
	)

	It("leaves a real title that merely mentions a filler word alone", func() {
		Expect(summaryProblems(AgentSummary{
			Title:    "fix: drop TODO",
			Sections: []SummarySection{{Name: "Testing", Content: "ran"}},
		}, content)).To(BeEmpty())
	})
})

var _ = Describe("renderBody", func() {
	declared := []forgeconfig.Section{{Name: "Testing", Required: true}, {Name: "Risks"}}

	It("returns the description alone when nothing is declared", func() {
		Expect(renderBody("Did the thing.", nil, nil)).To(Equal("Did the thing."))
	})

	It("renders sections in declared order, not the order they came back in", func() {
		out := renderBody("Did the thing.", declared, []SummarySection{
			{Name: "Risks", Content: "None."},
			{Name: "Testing", Content: "go test ./..."},
		})

		Expect(out).To(Equal("Did the thing.\n\n## Testing\n\ngo test ./...\n\n## Risks\n\nNone."))
	})

	It("matches a returned section name case-insensitively", func() {
		out := renderBody("x", declared, []SummarySection{{Name: "testing", Content: "ran"}})
		Expect(out).To(ContainSubstring("## Testing\n\nran"))
	})

	It("drops an optional section that came back empty", func() {
		out := renderBody("x", declared, []SummarySection{{Name: "Testing", Content: "ran"}})
		Expect(out).NotTo(ContainSubstring("## Risks"))
	})

	It("renders a missing required section as explicitly not provided", func() {
		out := renderBody("x", declared, nil)
		Expect(out).To(ContainSubstring("## Testing\n\n" + notProvided))
	})

	It("ignores sections nobody asked for", func() {
		out := renderBody("x", declared, []SummarySection{
			{Name: "Testing", Content: "ran"},
			{Name: "Invented", Content: "should not appear"},
		})
		Expect(out).NotTo(ContainSubstring("Invented"))
	})
})

var _ = Describe("ApplyFooter", func() {
	It("returns the body untouched when no footer is configured", func() {
		Expect(ApplyFooter("Did the thing.", "")).To(Equal("Did the thing."))
	})

	It("separates the footer from the body with a blank line", func() {
		Expect(ApplyFooter("Did the thing.", "Generated by kvarn")).To(
			Equal("Did the thing.\n\nGenerated by kvarn"))
	})

	It("stands alone when the body is empty", func() {
		Expect(ApplyFooter("  ", "Generated by kvarn")).To(Equal("Generated by kvarn"))
	})
})

var _ = Describe("ApplyTrailers", func() {
	It("returns the message untouched when none are configured", func() {
		Expect(ApplyTrailers("fix: thing\n\nbody", nil)).To(Equal("fix: thing\n\nbody"))
	})

	It("appends them as a block separated by a blank line", func() {
		Expect(ApplyTrailers("fix: thing\n\nbody", []string{"Kvarn-Session: abc", "Refs: #12"})).To(
			Equal("fix: thing\n\nbody\n\nKvarn-Session: abc\nRefs: #12"))
	})

	It("skips blank entries", func() {
		Expect(ApplyTrailers("fix: thing", []string{"  ", "Kvarn-Session: abc"})).To(
			Equal("fix: thing\n\nKvarn-Session: abc"))
	})
})

var _ = Describe("Mode comment conventions", func() {
	content := forgeconfig.Content{
		CommentInstructions: forgeconfig.Instructions{Organization: "Lead with the verdict."},
		CommentSections:     []forgeconfig.Section{{Name: "Findings", Description: "One bullet each."}},
	}

	It("reaches a mode whose written result is posted as a comment", func() {
		mode := &Mode{Name: "review-pr", Deliver: []Sink{SinkPRComment}, role: "a reviewer", body: "x"}
		out := mode.SystemPrompt("p", "r", "b", nil, nil, content)

		Expect(out).To(ContainSubstring("## Comment conventions"))
		Expect(out).To(ContainSubstring("Lead with the verdict."))
		Expect(out).To(ContainSubstring("- Findings: One bullet each."))
	})

	It("is left out of a mode that delivers no comment", func() {
		Expect(ModeImplement.SystemPrompt("p", "r", "b", nil, nil, content)).
			NotTo(ContainSubstring("## Comment conventions"))
	})

	It("is left out when nothing is configured", func() {
		mode := &Mode{Name: "review-pr", Deliver: []Sink{SinkPRComment}, role: "a reviewer", body: "x"}
		Expect(mode.SystemPrompt("p", "r", "b", nil, nil, forgeconfig.Content{})).
			NotTo(ContainSubstring("## Comment conventions"))
	})
})
