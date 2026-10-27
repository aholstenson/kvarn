package jobs

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("loadValidation", func() {
	write := func(body string) string {
		path := filepath.Join(GinkgoT().TempDir(), "validation.yml")
		Expect(os.WriteFile(path, []byte(body), 0o644)).To(Succeed())
		return path
	}

	It("reads the same shape as the kvarn.yml block", func() {
		v, err := loadValidation(write(`
required:
  - name: Acceptance
    prompt: Check that the export downloads a CSV.
    timeout: 10m
advisory:
  - name: Lint
    run: npm run lint
    paths: ["web/**"]
`))
		Expect(err).NotTo(HaveOccurred())
		Expect(v.GetRequired()).To(HaveLen(1))
		Expect(v.GetRequired()[0].GetPrompt()).To(ContainSubstring("CSV"))
		Expect(v.GetRequired()[0].GetTimeoutSeconds()).To(Equal(uint32(600)))
		Expect(v.GetAdvisory()[0].GetRun()).To(Equal("npm run lint"))
		Expect(v.GetAdvisory()[0].GetPaths()).To(Equal([]string{"web/**"}))
	})

	It("rejects an unknown key rather than dropping it", func() {
		_, err := loadValidation(write("required:\n  - name: A\n    promt: typo\n"))
		Expect(err).To(HaveOccurred())
	})

	It("rejects a step with both run and prompt before submitting", func() {
		_, err := loadValidation(write("required:\n  - name: A\n    run: make\n    prompt: Check.\n"))
		Expect(err).To(MatchError(ContainSubstring("both run and prompt")))
	})

	It("rejects retry, which only setup steps honour", func() {
		_, err := loadValidation(write("required:\n  - name: A\n    run: make\n    retry: 2\n"))
		Expect(err).To(MatchError(ContainSubstring("retry applies only to setup steps")))
	})
})
