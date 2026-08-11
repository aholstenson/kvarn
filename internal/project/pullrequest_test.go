package project_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/project"
)

// writeConfig writes a kvarn.yml into a fresh directory and loads it.
func writeConfig(yaml string) (*project.Config, error) {
	dir := GinkgoT().TempDir()
	Expect(os.WriteFile(filepath.Join(dir, "kvarn.yml"), []byte(yaml), 0o644)).To(Succeed())
	return project.Load(dir)
}

var _ = Describe("pull_request", func() {
	It("is absent from a config that does not declare it", func() {
		cfg, err := writeConfig("vm:\n  cpus: 2\n")
		Expect(err).NotTo(HaveOccurred())

		c := cfg.PullRequest.Resolve("implement")
		Expect(c.TitleInstructions).To(BeEmpty())
		Expect(c.BodySections).To(BeEmpty())
	})

	It("parses the whole block", func() {
		cfg, err := writeConfig(`
pull_request:
  title:
    instructions: Use Conventional Commits.
    max_length: 60
  body:
    instructions: Write for a reviewer who has not seen the task.
    sections:
      - name: Testing
        description: Which commands ran and what they reported.
        required: true
      - name: Risks
  comment:
    instructions: Lead with the verdict.
    sections:
      - name: Verdict
`)
		Expect(err).NotTo(HaveOccurred())

		c := cfg.PullRequest.Resolve("")
		Expect(c.TitleInstructions).To(Equal("Use Conventional Commits."))
		Expect(*c.TitleMaxLength).To(Equal(60))
		Expect(c.BodyInstructions).To(Equal("Write for a reviewer who has not seen the task."))
		Expect(c.BodySections).To(HaveLen(2))
		Expect(c.BodySections[0].Name).To(Equal("Testing"))
		Expect(c.BodySections[0].Required).To(BeTrue())
		Expect(c.BodySections[1].Name).To(Equal("Risks"))
		Expect(c.BodySections[1].Required).To(BeFalse())
		Expect(c.CommentInstructions).To(Equal("Lead with the verdict."))
		Expect(c.CommentSections).To(HaveLen(1))
	})

	Describe("per-mode overrides", func() {
		const cfgYAML = `
pull_request:
  body:
    instructions: Base guidance.
    sections:
      - name: Testing
        description: Base description.
      - name: Risks
  modes:
    implement:
      body:
        instructions: Also list new dependencies.
        sections:
          - name: Testing
            description: Mode description.
            required: true
          - name: Migration
`

		It("leaves the top-level block alone for a mode with no entry", func() {
			cfg, err := writeConfig(cfgYAML)
			Expect(err).NotTo(HaveOccurred())

			c := cfg.PullRequest.Resolve("review")
			Expect(c.BodyInstructions).To(Equal("Base guidance."))
			Expect(c.BodySections).To(HaveLen(2))
		})

		It("concatenates instructions rather than replacing them", func() {
			cfg, err := writeConfig(cfgYAML)
			Expect(err).NotTo(HaveOccurred())

			Expect(cfg.PullRequest.Resolve("implement").BodyInstructions).To(
				Equal("Base guidance.\n\nAlso list new dependencies."))
		})

		It("replaces a section it shares a name with, in place, and appends the rest", func() {
			cfg, err := writeConfig(cfgYAML)
			Expect(err).NotTo(HaveOccurred())

			s := cfg.PullRequest.Resolve("implement").BodySections
			Expect(s).To(HaveLen(3))
			Expect(s[0].Name).To(Equal("Testing"))
			Expect(s[0].Description).To(Equal("Mode description."))
			Expect(s[0].Required).To(BeTrue())
			Expect(s[1].Name).To(Equal("Risks"), "an untouched section keeps its position")
			Expect(s[2].Name).To(Equal("Migration"), "a new one is appended")
		})

		It("does not leak one mode's additions into another", func() {
			cfg, err := writeConfig(cfgYAML)
			Expect(err).NotTo(HaveOccurred())

			Expect(cfg.PullRequest.Resolve("implement").BodySections).To(HaveLen(3))
			Expect(cfg.PullRequest.Resolve("review").BodySections).To(HaveLen(2))
		})
	})

	Describe("validation", func() {
		It("rejects a section with no name", func() {
			_, err := writeConfig("pull_request:\n  body:\n    sections:\n      - description: no name here\n")
			Expect(err).To(MatchError(ContainSubstring("body.sections contains an entry with no name")))
		})

		It("rejects duplicate section names regardless of case", func() {
			_, err := writeConfig(`
pull_request:
  body:
    sections:
      - name: Testing
      - name: testing
`)
			Expect(err).To(MatchError(ContainSubstring(`"testing" is duplicated`)))
		})

		It("rejects a section name that spans lines", func() {
			_, err := writeConfig("pull_request:\n  comment:\n    sections:\n      - name: \"one\\ntwo\"\n")
			Expect(err).To(MatchError(ContainSubstring("must be a single line")))
		})

		It("rejects a non-positive title budget", func() {
			_, err := writeConfig("pull_request:\n  title:\n    max_length: 0\n")
			Expect(err).To(MatchError(ContainSubstring("title.max_length must be positive")))
		})

		It("rejects a mode key that is not a mode name", func() {
			_, err := writeConfig("pull_request:\n  modes:\n    Not A Mode:\n      body:\n        instructions: x\n")
			Expect(err).To(MatchError(ContainSubstring("lowercase alphanumerics")))
		})

		It("names the mode a nested error came from", func() {
			_, err := writeConfig(`
pull_request:
  modes:
    implement:
      body:
        sections:
          - description: nameless
`)
			Expect(err).To(MatchError(ContainSubstring("modes.implement:")))
		})
	})
})
