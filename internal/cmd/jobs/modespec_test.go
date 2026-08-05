package jobs

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("loadModeSpec", func() {
	var dir string

	BeforeEach(func() {
		var err error
		dir, err = os.MkdirTemp("", "modespec-test-*")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		os.RemoveAll(dir)
	})

	write := func(body string) string {
		path := filepath.Join(dir, "mode.yml")
		Expect(os.WriteFile(path, []byte(body), 0o644)).To(Succeed())
		return path
	}

	It("reads every axis", func() {
		spec, err := loadModeSpec(write(`
name: review-pr
extends: review
start: pull-request
deliver:
  - pr-comment
context:
  - pr-diff
`))
		Expect(err).NotTo(HaveOccurred())
		Expect(spec.GetName()).To(Equal("review-pr"))
		Expect(spec.GetDeliver()).To(Equal([]string{"pr-comment"}))
		Expect(spec.GetContext()).To(Equal([]string{"pr-diff"}))
	})

	// An empty list cannot cross the wire as anything but an absent one, so it
	// is refused here rather than reaching the orchestrator as "inherit".
	It("rejects an empty deliver list", func() {
		_, err := loadModeSpec(write("extends: auto\ndeliver: []\n"))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("deliver: [none]"))
	})

	It("rejects an empty context list", func() {
		_, err := loadModeSpec(write("extends: feedback\ncontext: []\n"))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("context: [none]"))
	})

	It("rejects an unknown key rather than dropping it", func() {
		_, err := loadModeSpec(write("extends: review\nworkspce: read-only\n"))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("parse mode definition"))
	})
})
