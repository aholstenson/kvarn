// Package limits resolves the layered cost-limit configuration for a single
// job run. The resolver collapses (project, defaults, built-in) inputs into a
// single Limits value the orchestrator can hand to a cost.Tracker.
package limits

import (
	modelcfg "github.com/aholstenson/kvarn/internal/config/model"
	"github.com/aholstenson/kvarn/internal/config/project"
)

// Built-in fallbacks. These apply only when neither project nor defaults
// configure a value; they keep a brand-new install from running forever.
const (
	BuiltinMaxCostUSD           = 5.00
	BuiltinWarnThreshold        = 0.80
	BuiltinReportCostOnPR       = true
	BuiltinMaxValidationRetries = 3
)

// Limits is the resolved per-job configuration. MaxCostUSD is the hard cap;
// WarnThreshold is the fraction of MaxCostUSD at which the soft warning
// fires; ReportCostOnPR decides whether the work-log PR comment shows the
// cost section; MaxValidationRetries is the maximum number of additional
// agent passes allowed after the first when required validation fails.
type Limits struct {
	MaxCostUSD           float64
	WarnThreshold        float64
	ReportCostOnPR       bool
	MaxValidationRetries int
}

// Resolve produces a Limits value for the given (project, defaults, mode)
// triple following the five-step rule documented on internal/agent/cost:
//
//  1. projects.<name>.jobs.<mode>.max_cost_usd
//  2. projects.<name>.max_cost_usd
//  3. defaults.jobs.<mode>.max_cost_usd
//  4. defaults.max_cost_usd
//  5. built-in fallback (BuiltinMaxCostUSD)
//
// ReportCostOnPR resolves project → defaults → built-in. WarnThreshold is
// user-level only (defaults → built-in); there is no project knob.
//
// MaxValidationRetries follows the same five-step rule as MaxCostUSD
// (project.jobs.<mode> → project → defaults.jobs.<mode> → defaults → builtin).
//
// Both arguments may be nil/zero. A nil project means "no project-level
// overrides"; a zero Defaults means "no user-level config".
func Resolve(proj *project.Project, defaults modelcfg.Defaults, mode string) Limits {
	out := Limits{
		MaxCostUSD:           BuiltinMaxCostUSD,
		WarnThreshold:        BuiltinWarnThreshold,
		ReportCostOnPR:       BuiltinReportCostOnPR,
		MaxValidationRetries: BuiltinMaxValidationRetries,
	}

	if defaults.MaxCostUSD != nil {
		out.MaxCostUSD = *defaults.MaxCostUSD
	}
	if mode != "" {
		if j, ok := defaults.Jobs[mode]; ok && j.MaxCostUSD != nil {
			out.MaxCostUSD = *j.MaxCostUSD
		}
	}

	if proj != nil {
		if proj.MaxCostUSD != nil {
			out.MaxCostUSD = *proj.MaxCostUSD
		}
		if mode != "" {
			if j, ok := proj.Jobs[mode]; ok && j.MaxCostUSD != nil {
				out.MaxCostUSD = *j.MaxCostUSD
			}
		}
	}

	if defaults.MaxValidationRetries != nil {
		out.MaxValidationRetries = *defaults.MaxValidationRetries
	}
	if mode != "" {
		if j, ok := defaults.Jobs[mode]; ok && j.MaxValidationRetries != nil {
			out.MaxValidationRetries = *j.MaxValidationRetries
		}
	}
	if proj != nil {
		if proj.MaxValidationRetries != nil {
			out.MaxValidationRetries = *proj.MaxValidationRetries
		}
		if mode != "" {
			if j, ok := proj.Jobs[mode]; ok && j.MaxValidationRetries != nil {
				out.MaxValidationRetries = *j.MaxValidationRetries
			}
		}
	}

	if defaults.WarnThreshold != nil {
		out.WarnThreshold = *defaults.WarnThreshold
	}

	if defaults.ReportCostOnPR != nil {
		out.ReportCostOnPR = *defaults.ReportCostOnPR
	}
	if proj != nil && proj.ReportCostOnPR != nil {
		out.ReportCostOnPR = *proj.ReportCostOnPR
	}

	return out
}
