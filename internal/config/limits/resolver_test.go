package limits_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/config/limits"
	modelcfg "github.com/aholstenson/kvarn/internal/config/model"
	"github.com/aholstenson/kvarn/internal/config/project"
)

func f(v float64) *float64 { return &v }
func b(v bool) *bool       { return &v }

var _ = Describe("Resolve", func() {
	It("falls back to built-ins when nothing is set", func() {
		out := limits.Resolve(nil, modelcfg.Defaults{}, "implement")
		Expect(out.MaxCostUSD).To(Equal(limits.BuiltinMaxCostUSD))
		Expect(out.WarnThreshold).To(Equal(limits.BuiltinWarnThreshold))
		Expect(out.ReportCostOnPR).To(Equal(limits.BuiltinReportCostOnPR))
	})

	It("uses defaults.max_cost_usd when set", func() {
		out := limits.Resolve(nil, modelcfg.Defaults{MaxCostUSD: f(10)}, "implement")
		Expect(out.MaxCostUSD).To(Equal(10.0))
	})

	It("uses defaults.jobs.<mode>.max_cost_usd over defaults.max_cost_usd", func() {
		defaults := modelcfg.Defaults{
			MaxCostUSD: f(10),
			Jobs: map[string]modelcfg.JobDefaults{
				"implement": {MaxCostUSD: f(25)},
			},
		}
		Expect(limits.Resolve(nil, defaults, "implement").MaxCostUSD).To(Equal(25.0))
		Expect(limits.Resolve(nil, defaults, "fix").MaxCostUSD).To(Equal(10.0))
	})

	It("uses project.max_cost_usd over defaults", func() {
		defaults := modelcfg.Defaults{
			MaxCostUSD: f(10),
			Jobs:       map[string]modelcfg.JobDefaults{"implement": {MaxCostUSD: f(25)}},
		}
		proj := &project.Project{MaxCostUSD: f(50)}
		Expect(limits.Resolve(proj, defaults, "implement").MaxCostUSD).To(Equal(50.0))
	})

	It("uses project.jobs.<mode>.max_cost_usd over project.max_cost_usd", func() {
		defaults := modelcfg.Defaults{
			MaxCostUSD: f(10),
			Jobs:       map[string]modelcfg.JobDefaults{"implement": {MaxCostUSD: f(25)}},
		}
		proj := &project.Project{
			MaxCostUSD: f(50),
			Jobs:       map[string]project.JobLimits{"implement": {MaxCostUSD: f(100)}},
		}
		Expect(limits.Resolve(proj, defaults, "implement").MaxCostUSD).To(Equal(100.0))
		// A different mode falls back to project.MaxCostUSD.
		Expect(limits.Resolve(proj, defaults, "fix").MaxCostUSD).To(Equal(50.0))
	})

	It("resolves ReportCostOnPR project → defaults → built-in", func() {
		// Built-in.
		Expect(limits.Resolve(nil, modelcfg.Defaults{}, "implement").ReportCostOnPR).
			To(Equal(limits.BuiltinReportCostOnPR))
		// Defaults override built-in.
		Expect(limits.Resolve(nil, modelcfg.Defaults{ReportCostOnPR: b(false)}, "implement").ReportCostOnPR).
			To(BeFalse())
		// Project overrides defaults.
		proj := &project.Project{ReportCostOnPR: b(true)}
		Expect(limits.Resolve(proj, modelcfg.Defaults{ReportCostOnPR: b(false)}, "implement").ReportCostOnPR).
			To(BeTrue())
	})

	It("WarnThreshold is user-level only", func() {
		out := limits.Resolve(nil, modelcfg.Defaults{WarnThreshold: f(0.5)}, "implement")
		Expect(out.WarnThreshold).To(Equal(0.5))
	})
})
