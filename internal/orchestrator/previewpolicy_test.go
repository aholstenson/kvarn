package orchestrator

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	orchcfg "github.com/aholstenson/kvarn/internal/config/orchestrator"
)

// ptr is a tiny helper for the pointer-valued config fields.
func ptr[T any](v T) *T { return &v }

var _ = Describe("resolvePreviewPolicy", func() {
	It("disables previews when the section is absent", func() {
		policy, err := resolvePreviewPolicy(orchcfg.Preview{})
		Expect(err).NotTo(HaveOccurred())
		Expect(policy.Enabled()).To(BeFalse())
	})

	It("applies the built-in defaults for a minimal section", func() {
		policy, err := resolvePreviewPolicy(orchcfg.Preview{
			Domain: "preview.example.com",
			Listen: "100.64.0.1:8080",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(policy.Enabled()).To(BeTrue())
		Expect(policy.Domain).To(Equal("preview.example.com"))
		Expect(policy.IdleTimeout).To(Equal(defaultPreviewIdleTimeout))
		Expect(policy.MaxLifetime).To(Equal(defaultPreviewMaxLifetime))
		Expect(policy.MaxConcurrent).To(Equal(defaultPreviewMaxConcurrent))
		Expect(policy.MaxMemoryBytes).To(BeZero())
		Expect(policy.MaxDiskBytes).To(BeZero())
	})

	It("reads every field the operator set", func() {
		policy, err := resolvePreviewPolicy(orchcfg.Preview{
			Domain:        "preview.example.com",
			Listen:        "100.64.0.1:8080",
			IdleTimeout:   "45m",
			MaxLifetime:   "12h",
			MaxConcurrent: ptr(5),
			MaxMemory:     "8G",
			MaxDisk:       "64G",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(policy.IdleTimeout).To(Equal(45 * time.Minute))
		Expect(policy.MaxLifetime).To(Equal(12 * time.Hour))
		Expect(policy.MaxConcurrent).To(Equal(5))
		Expect(policy.MaxMemoryBytes).To(Equal(uint64(8) * 1024 * 1024 * 1024))
		Expect(policy.MaxDiskBytes).To(Equal(int64(64) * 1024 * 1024 * 1024))
	})

	It("trims a domain written with trailing dots", func() {
		policy, err := resolvePreviewPolicy(orchcfg.Preview{
			Domain: ".preview.example.com.",
			Listen: "100.64.0.1:8080",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(policy.Domain).To(Equal("preview.example.com"))
	})

	It("treats an explicit zero as disabling that cap", func() {
		policy, err := resolvePreviewPolicy(orchcfg.Preview{
			Domain:        "preview.example.com",
			Listen:        "100.64.0.1:8080",
			IdleTimeout:   "0",
			MaxLifetime:   "0",
			MaxConcurrent: ptr(0),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(policy.IdleTimeout).To(BeZero())
		Expect(policy.MaxLifetime).To(BeZero())
		Expect(policy.MaxConcurrent).To(BeZero())
	})

	It("refuses a domain with no listener", func() {
		_, err := resolvePreviewPolicy(orchcfg.Preview{Domain: "preview.example.com"})
		Expect(err).To(MatchError(ContainSubstring("unreachable")))
	})

	It("refuses a listener with no domain", func() {
		_, err := resolvePreviewPolicy(orchcfg.Preview{Listen: "100.64.0.1:8080"})
		Expect(err).To(MatchError(ContainSubstring("unaddressable")))
	})

	DescribeTable("rejects a malformed value",
		func(cfg orchcfg.Preview, want string) {
			cfg.Domain = "preview.example.com"
			cfg.Listen = "100.64.0.1:8080"
			_, err := resolvePreviewPolicy(cfg)
			Expect(err).To(MatchError(ContainSubstring(want)))
		},
		Entry("an unparseable idle timeout", orchcfg.Preview{IdleTimeout: "half an hour"}, "idle_timeout"),
		Entry("a negative idle timeout", orchcfg.Preview{IdleTimeout: "-5m"}, "must be non-negative"),
		Entry("an unparseable max lifetime", orchcfg.Preview{MaxLifetime: "forever"}, "max_lifetime"),
		Entry("a negative max_concurrent", orchcfg.Preview{MaxConcurrent: ptr(-1)}, "must not be negative"),
		Entry("an unparseable max memory", orchcfg.Preview{MaxMemory: "lots"}, "max_memory"),
		Entry("an unparseable max disk", orchcfg.Preview{MaxDisk: "8TB"}, "max_disk"),
	)
})
