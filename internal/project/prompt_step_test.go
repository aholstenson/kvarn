package project_test

import (
	"strings"

	"github.com/aholstenson/kvarn/internal/project"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Prompt validation steps", func() {
	var dir string

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
	})

	load := func(body string) (*project.Config, error) {
		writeYAML(dir, "kvarn.yml", body)
		return project.Load(dir)
	}

	It("parses a prompt step beside a command step", func() {
		cfg, err := load(`
validation:
  required:
    - name: Tests
      run: npm test
    - name: Acceptance
      prompt: |
        Check that the export button downloads a CSV.
      timeout: 15m
      paths:
        - "web/**"
`)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Validation.Required).To(HaveLen(2))
		step := cfg.Validation.Required[1]
		Expect(step.IsPrompt()).To(BeTrue())
		Expect(step.Prompt).To(ContainSubstring("export button"))
		Expect(step.Timeout.Seconds()).To(Equal(uint32(900)))
		Expect(step.Paths).To(Equal([]string{"web/**"}))
		Expect(cfg.Validation.Required[0].IsPrompt()).To(BeFalse())
	})

	DescribeTable("rejects a prompt where it does not belong",
		func(body, wantErr string) {
			_, err := load(body)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(wantErr))
		},
		Entry("both run and prompt", `
validation:
  required:
    - name: Both
      run: npm test
      prompt: Check it.
`, "both run and prompt"),
		Entry("a setup step", `
setup:
  steps:
    - name: Install
      prompt: Install what is needed.
`, "only validation steps"),
		Entry("a health check", `
setup:
  health_checks:
    - name: Up
      prompt: Check the server is up.
`, "only validation steps"),
		Entry("working_dir on a prompt step", `
validation:
  advisory:
    - name: Style
      prompt: Check the style.
      working_dir: web
`, "working_dir applies only to run commands"),
		Entry("a preview step", `
preview:
  sites:
    web: { port: 3000 }
  serve:
    - { name: Web, run: npm start }
  setup:
    - name: Seed
      prompt: Seed the database.
`, "only validation steps"),
	)

	It("rejects a prompt over the size limit", func() {
		big := strings.Repeat("x", project.MaxPromptBytes+1)
		v := project.Validation{Required: []project.Step{{Name: "Big", Prompt: big}}}
		Expect(v.Validate()).To(MatchError(ContainSubstring("the limit is")))
	})

	It("still requires a run command on a step without a prompt", func() {
		_, err := load(`
validation:
  required:
    - name: Empty
`)
		Expect(err).To(MatchError(ContainSubstring("empty run command")))
	})
})
