package tomlstore_test

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	llms "github.com/aholstenson/llms-go"

	"github.com/aholstenson/kvarn/internal/config/model/tomlstore"
)

var _ = Describe("Model TomlStore", func() {
	var (
		ctx    context.Context
		tmpDir string
		path   string
		store  *tomlstore.Store
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		tmpDir, err = os.MkdirTemp("", "model-store-test-*")
		Expect(err).NotTo(HaveOccurred())
		path = filepath.Join(tmpDir, "agents.toml")
		store = tomlstore.New(path)
	})

	AfterEach(func() {
		os.RemoveAll(tmpDir)
	})

	It("returns empty maps when the file does not exist", func() {
		all, err := store.All(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(BeEmpty())

		d, err := store.Defaults(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.MaxCostUSD).To(BeNil())
		Expect(d.Jobs).To(BeNil())
	})

	It("surfaces a parse error from All rather than silently returning empty", func() {
		Expect(os.WriteFile(path, []byte("not = valid = toml"), 0o644)).To(Succeed())

		_, err := store.All(ctx)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("parse "))
	})

	It("surfaces a parse error from Defaults rather than silently returning empty", func() {
		Expect(os.WriteFile(path, []byte("not = valid = toml"), 0o644)).To(Succeed())

		_, err := store.Defaults(ctx)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("parse "))
	})

	It("round-trips the [defaults] block including per-job overrides", func() {
		content := `[defaults]
max_cost_usd          = 5.0
warn_threshold        = 0.8
report_cost_on_pr     = true
max_validation_retries = 3

[defaults.jobs.implement]
max_cost_usd = 25.0
max_validation_retries = 1
`
		Expect(os.WriteFile(path, []byte(content), 0o644)).To(Succeed())

		d, err := store.Defaults(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.MaxCostUSD).NotTo(BeNil())
		Expect(*d.MaxCostUSD).To(Equal(5.0))
		Expect(d.WarnThreshold).NotTo(BeNil())
		Expect(*d.WarnThreshold).To(Equal(0.8))
		Expect(d.ReportCostOnPR).NotTo(BeNil())
		Expect(*d.ReportCostOnPR).To(BeTrue())
		Expect(d.MaxValidationRetries).NotTo(BeNil())
		Expect(*d.MaxValidationRetries).To(Equal(3))

		Expect(d.Jobs).To(HaveKey("implement"))
		impl := d.Jobs["implement"]
		Expect(impl.MaxCostUSD).NotTo(BeNil())
		Expect(*impl.MaxCostUSD).To(Equal(25.0))
		Expect(impl.MaxValidationRetries).NotTo(BeNil())
		Expect(*impl.MaxValidationRetries).To(Equal(1))
	})

	It("reads [models.<alias>] entries via All", func() {
		content := `[models.coder]
model            = "anthropic/claude-sonnet-4-6"
reasoning_effort = "medium"
max_output_tokens = 16384
`
		Expect(os.WriteFile(path, []byte(content), 0o644)).To(Succeed())

		all, err := store.All(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(HaveKey("coder"))
		entry := all["coder"]
		Expect(entry.ModelID).To(Equal("anthropic/claude-sonnet-4-6"))
		Expect(entry.ReasoningEffort).NotTo(BeNil())
		Expect(*entry.ReasoningEffort).To(Equal(llms.Effort("medium")))
		Expect(entry.MaxOutputTokens).NotTo(BeNil())
		Expect(*entry.MaxOutputTokens).To(Equal(16384))
	})
})
