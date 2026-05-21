package coding_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/agent/coding"
)

var _ = Describe("ModeByName", func() {
	It("returns ModeAuto for empty input", func() {
		m, err := coding.ModeByName("")
		Expect(err).NotTo(HaveOccurred())
		Expect(m).To(Equal(coding.ModeAuto))
	})

	It("returns ModeAuto for 'auto'", func() {
		m, err := coding.ModeByName("auto")
		Expect(err).NotTo(HaveOccurred())
		Expect(m).To(Equal(coding.ModeAuto))
	})

	It("returns ModeImplement for 'implement'", func() {
		m, err := coding.ModeByName("implement")
		Expect(err).NotTo(HaveOccurred())
		Expect(m).To(Equal(coding.ModeImplement))
	})

	It("returns ModeFix for 'fix'", func() {
		m, err := coding.ModeByName("fix")
		Expect(err).NotTo(HaveOccurred())
		Expect(m).To(Equal(coding.ModeFix))
	})

	It("returns ModeReview for 'review'", func() {
		m, err := coding.ModeByName("review")
		Expect(err).NotTo(HaveOccurred())
		Expect(m).To(Equal(coding.ModeReview))
	})

	It("returns ModeResearch for 'research'", func() {
		m, err := coding.ModeByName("research")
		Expect(err).NotTo(HaveOccurred())
		Expect(m).To(Equal(coding.ModeResearch))
	})

	It("is case-insensitive and trims whitespace", func() {
		m, err := coding.ModeByName("  REVIEW  ")
		Expect(err).NotTo(HaveOccurred())
		Expect(m).To(Equal(coding.ModeReview))
	})

	It("returns an error for an unknown mode", func() {
		_, err := coding.ModeByName("bogus")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("bogus"))
	})

	It("returns an error for the removed 'deliberate' mode", func() {
		_, err := coding.ModeByName("deliberate")
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("Mode.WritesChanges", func() {
	It("is true for auto", func() {
		Expect(coding.ModeAuto.WritesChanges()).To(BeTrue())
	})

	It("is true for implement", func() {
		Expect(coding.ModeImplement.WritesChanges()).To(BeTrue())
	})

	It("is true for fix", func() {
		Expect(coding.ModeFix.WritesChanges()).To(BeTrue())
	})

	It("is false for review", func() {
		Expect(coding.ModeReview.WritesChanges()).To(BeFalse())
	})

	It("is false for research", func() {
		Expect(coding.ModeResearch.WritesChanges()).To(BeFalse())
	})
})
