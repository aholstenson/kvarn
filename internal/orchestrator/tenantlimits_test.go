package orchestrator

import (
	"github.com/aholstenson/kvarn/internal/config/apikey"
	orchcfg "github.com/aholstenson/kvarn/internal/config/orchestrator"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/orchestrator/scheduler"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func intp(v int) *int    { return &v }
func uintp(v uint) *uint { return &v }

var _ = Describe("Tenant limits", func() {
	hostDefaults := scheduler.Limits{
		MaxJobs: 4,
		Max: scheduler.Capacity{
			CPUMillis: 8000,
			MemBytes:  16 * 1024 * 1024 * 1024,
		},
	}

	Describe("resolveTenantLimits", func() {
		It("is uncapped when the table is absent", func() {
			out, err := resolveTenantLimits(orchcfg.Scheduler{})
			Expect(err).NotTo(HaveOccurred())
			Expect(out.PerProject.IsZero()).To(BeTrue())
			Expect(out.PerKey.IsZero()).To(BeTrue())
		})

		It("parses both scopes", func() {
			out, err := resolveTenantLimits(orchcfg.Scheduler{
				PerProject: orchcfg.TenantLimits{MaxJobs: intp(3), MaxMemory: "16G"},
				PerKey:     orchcfg.TenantLimits{MaxCPUs: uintp(8), MaxDisk: "100G"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(out.PerProject.MaxJobs).To(Equal(3))
			Expect(out.PerProject.Max.MemBytes).To(Equal(uint64(16) * 1024 * 1024 * 1024))
			Expect(out.PerKey.Max.CPUMillis).To(Equal(uint64(8000)))
			Expect(out.PerKey.Max.DiskBytes).To(Equal(uint64(100) * 1024 * 1024 * 1024))
		})

		It("reports which scope failed to parse", func() {
			_, err := resolveTenantLimits(orchcfg.Scheduler{
				PerKey: orchcfg.TenantLimits{MaxMemory: "not-a-size"},
			})
			Expect(err).To(MatchError(ContainSubstring("per_key")))
			Expect(err).To(MatchError(ContainSubstring("max_memory")))
		})
	})

	Describe("projectLimits", func() {
		It("inherits the host default when the project sets nothing", func() {
			out, err := projectLimits(&project.Project{Name: "alpha"}, hostDefaults)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal(hostDefaults))
		})

		It("inherits field by field rather than all or nothing", func() {
			out, err := projectLimits(&project.Project{Name: "alpha", MaxJobs: intp(1)}, hostDefaults)
			Expect(err).NotTo(HaveOccurred())
			Expect(out.MaxJobs).To(Equal(1))
			Expect(out.Max.CPUMillis).To(Equal(hostDefaults.Max.CPUMillis), "the untouched dimension still inherits")
			Expect(out.Max.MemBytes).To(Equal(hostDefaults.Max.MemBytes))
		})

		It("treats an explicit zero as opting out of the host default", func() {
			out, err := projectLimits(&project.Project{Name: "alpha", MaxJobs: intp(0), MaxCPUs: uintp(0)}, hostDefaults)
			Expect(err).NotTo(HaveOccurred())
			Expect(out.MaxJobs).To(Equal(0))
			Expect(out.Max.CPUMillis).To(Equal(uint64(0)))
			Expect(out.Max.MemBytes).To(Equal(hostDefaults.Max.MemBytes))
		})

		It("rejects a negative job cap", func() {
			_, err := projectLimits(&project.Project{Name: "alpha", MaxJobs: intp(-1)}, scheduler.Limits{})
			Expect(err).To(MatchError(ContainSubstring(`project "alpha"`)))
			Expect(err).To(MatchError(ContainSubstring("max_jobs")))
		})

		It("names the project when a size fails to parse", func() {
			_, err := projectLimits(&project.Project{Name: "alpha", MaxDisk: "20 gigs"}, scheduler.Limits{})
			Expect(err).To(MatchError(ContainSubstring(`project "alpha"`)))
			Expect(err).To(MatchError(ContainSubstring("max_disk")))
		})

		It("falls back to the default for a nil project", func() {
			out, err := projectLimits(nil, hostDefaults)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal(hostDefaults))
		})
	})

	Describe("jobPriority", func() {
		It("is zero when nothing is configured", func() {
			Expect(jobPriority(&project.Project{Name: "alpha"}, "implement")).To(Equal(0))
			Expect(jobPriority(nil, "implement")).To(Equal(0))
		})

		It("uses the project's value", func() {
			Expect(jobPriority(&project.Project{Priority: intp(3)}, "implement")).To(Equal(3))
		})

		It("prefers the per-mode override", func() {
			p := &project.Project{
				Priority: intp(3),
				Jobs:     map[string]project.JobLimits{"feedback": {Priority: intp(9)}},
			}
			Expect(jobPriority(p, "feedback")).To(Equal(9))
			Expect(jobPriority(p, "implement")).To(Equal(3), "another mode still takes the project's value")
		})

		It("falls back to the project when the mode block sets no priority", func() {
			p := &project.Project{
				Priority: intp(3),
				Jobs:     map[string]project.JobLimits{"feedback": {MaxValidationRetries: intp(1)}},
			}
			Expect(jobPriority(p, "feedback")).To(Equal(3))
		})
	})

	Describe("keyLimits", func() {
		It("overrides the host default per field", func() {
			out, err := keyLimits(&apikey.APIKey{Name: "ci", MaxCPUs: uintp(2)}, hostDefaults)
			Expect(err).NotTo(HaveOccurred())
			Expect(out.Max.CPUMillis).To(Equal(uint64(2000)))
			Expect(out.MaxJobs).To(Equal(hostDefaults.MaxJobs))
		})

		It("names the key when a value is invalid", func() {
			_, err := keyLimits(&apikey.APIKey{Name: "ci", MaxMemory: "huge"}, scheduler.Limits{})
			Expect(err).To(MatchError(ContainSubstring(`key "ci"`)))
		})

		It("falls back to the default for a nil key", func() {
			out, err := keyLimits(nil, hostDefaults)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal(hostDefaults))
		})
	})
})
