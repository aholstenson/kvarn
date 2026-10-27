package coding

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Validator prompts", func() {
	It("gives the task and the criterion, and nothing from an earlier check on the first one", func() {
		prompt := validatorTaskPrompt("Add a CSV export.", "Acceptance", "Check the export downloads a CSV.", nil)
		Expect(prompt).To(ContainSubstring("Add a CSV export."))
		Expect(prompt).To(ContainSubstring("## Criterion: Acceptance"))
		Expect(prompt).To(ContainSubstring("Check the export downloads a CSV."))
		Expect(prompt).NotTo(ContainSubstring("last check"))
	})

	It("asks a later check to confirm the earlier findings first", func() {
		prompt := validatorTaskPrompt("Add a CSV export.", "Acceptance", "Check it.", []string{"The header row is missing."})
		Expect(prompt).To(ContainSubstring("## Findings from your last check"))
		Expect(prompt).To(ContainSubstring("- The header row is missing."))
		Expect(prompt).To(ContainSubstring("Check each one first"))
	})

	It("points the validator at the change from the run's base commit", func() {
		Expect(validatorSystemPrompt("shop", "abc123")).To(ContainSubstring("`git diff abc123`"))
		Expect(validatorSystemPrompt("shop", "")).To(ContainSubstring("`git diff HEAD`"))
	})
})

var _ = Describe("Validator findings", func() {
	It("renders each finding as a list item and indents its later lines", func() {
		Expect(renderFindings(false, []string{"First.", "Second\nwith detail."})).
			To(Equal("- First.\n- Second\n  with detail."))
	})

	It("says so when a failed check names no problem", func() {
		Expect(renderFindings(false, nil)).To(ContainSubstring("without naming a problem"))
		Expect(renderFindings(true, nil)).To(BeEmpty())
	})

	It("drops empty findings", func() {
		Expect(cleanFindings([]string{" ", "Real problem.", ""})).To(Equal([]string{"Real problem."}))
	})

	It("remembers findings per step between checks", func() {
		var s validatorState
		Expect(s.get("a")).To(BeNil())
		s.set("a", []string{"x"})
		Expect(s.get("a")).To(Equal([]string{"x"}))
		Expect(s.get("b")).To(BeNil())
	})
})
