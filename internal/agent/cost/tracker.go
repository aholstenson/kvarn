package cost

import (
	"context"
	"fmt"
	"sync"

	llms "github.com/aholstenson/llms-go"
)

// Tracker is the single source of truth for an in-flight job's LLM spend.
// It wraps an llms-go MetricsRecorder that captures per-service token usage as
// provider calls complete, and combines it with a PricingManager to translate
// tokens into USD. Snapshot, OverBudget, RemainingBudget, ConsumeWarning and
// CheckBudget are safe to call concurrently.
type Tracker struct {
	recorder *llms.DefaultMetricsRecorder
	pricing  *llms.PricingManager
	limit    Limit
	cancel   context.CancelCauseFunc

	mu             sync.Mutex
	warnFired      bool
	warnConsumed   bool
	overFired      bool
	onWarning      func(Report)
	onOverBudget   func(Report)
	lastWarnReport Report
}

// TrackerOpts configures a Tracker.
type TrackerOpts struct {
	// Pricing supplies USD-per-million rates. May be nil; without pricing, the
	// tracker still records token totals but USD always reads 0.
	Pricing *llms.PricingManager
	// Limit defines the budget. A zero MaxUSD disables enforcement.
	Limit Limit
	// Cancel is called once when MaxUSD is reached, with ErrBudgetExceeded as
	// the cause. May be nil; the caller is then responsible for noticing.
	Cancel context.CancelCauseFunc
	// OnWarning fires once, the first time spend crosses Limit.MaxUSD *
	// Limit.WarnFraction. The Report passed in is the snapshot at the moment
	// of crossing. May be nil.
	OnWarning func(Report)
	// OnOverBudget fires once, immediately before Cancel is invoked. May be
	// nil.
	OnOverBudget func(Report)
}

// NewTracker builds a Tracker from the given options.
func NewTracker(opts TrackerOpts) *Tracker {
	return &Tracker{
		recorder:     llms.NewMetricsRecorder(),
		pricing:      opts.Pricing,
		limit:        opts.Limit,
		cancel:       opts.Cancel,
		onWarning:    opts.OnWarning,
		onOverBudget: opts.OnOverBudget,
	}
}

// Recorder returns the underlying MetricsRecorder. Pass it through
// llms.WithMetrics so each provider call accrues to this tracker.
func (t *Tracker) Recorder() llms.MetricsRecorder { return t.recorder }

// Limit returns the configured budget. Useful for warning text and UI.
func (t *Tracker) Limit() Limit { return t.limit }

// Snapshot returns the current totals across all models, computed from the
// underlying recorder and the configured pricing manager.
func (t *Tracker) Snapshot() Report {
	stats := t.recorder.GetStats()
	var costs map[string]float64
	if t.pricing != nil {
		costs = t.pricing.CalculateCosts(stats)
	}

	perModel := make(map[string]ModelCost, len(stats.Success))
	var totalUSD float64
	var input, output, cached int64

	for service, counters := range stats.Success {
		in := int64(counters["input_tokens"])
		out := int64(counters["output_tokens"])
		c := int64(counters["cached_read_tokens"])
		var modelCost float64
		if costs != nil {
			modelCost = costs[service]
		}
		perModel[service] = ModelCost{
			ModelID:      service,
			InputTokens:  in,
			OutputTokens: out,
			CachedTokens: c,
			TotalUSD:     modelCost,
		}
		totalUSD += modelCost
		input += in
		output += out
		cached += c
	}

	return Report{
		InputTokens:  input,
		OutputTokens: output,
		CachedTokens: cached,
		TotalUSD:     totalUSD,
		PerModel:     perModel,
	}
}

// OverBudget reports whether the configured budget has been reached or
// exceeded. Returns false when no limit is set.
func (t *Tracker) OverBudget() bool {
	if t.limit.MaxUSD <= 0 {
		return false
	}
	return t.Snapshot().TotalUSD >= t.limit.MaxUSD
}

// RemainingBudget returns MaxUSD minus current spend. Negative when over
// budget. Returns 0 when no limit is set.
func (t *Tracker) RemainingBudget() float64 {
	if t.limit.MaxUSD <= 0 {
		return 0
	}
	return t.limit.MaxUSD - t.Snapshot().TotalUSD
}

// CheckBudget recomputes the snapshot, fires the warning callback if the warn
// threshold was just crossed, and triggers ctx cancellation when the hard
// limit is reached. Both transitions only ever fire once. Safe to call from
// any goroutine.
func (t *Tracker) CheckBudget() {
	if t.limit.MaxUSD <= 0 {
		return
	}
	report := t.Snapshot()

	t.mu.Lock()
	fireWarn := false
	fireOver := false

	if !t.overFired && report.TotalUSD >= t.limit.MaxUSD {
		t.overFired = true
		fireOver = true
	}

	if !t.warnFired && t.limit.WarnFraction > 0 {
		threshold := t.limit.MaxUSD * t.limit.WarnFraction
		if report.TotalUSD >= threshold {
			t.warnFired = true
			t.lastWarnReport = report
			fireWarn = true
		}
	}
	onWarning := t.onWarning
	onOverBudget := t.onOverBudget
	cancel := t.cancel
	t.mu.Unlock()

	if fireWarn && onWarning != nil {
		onWarning(report)
	}
	if fireOver {
		if onOverBudget != nil {
			onOverBudget(report)
		}
		if cancel != nil {
			cancel(ErrBudgetExceeded)
		}
	}
}

// ConsumeWarning returns the budget-warning note exactly once, after the warn
// threshold has been crossed. It is intended to be appended to the next tool
// result the model sees so the agent can self-regulate (finish current work,
// avoid spawning more sub-agents). Subsequent calls return ("", false).
func (t *Tracker) ConsumeWarning() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.warnFired || t.warnConsumed {
		return "", false
	}
	t.warnConsumed = true
	pct := 0.0
	if t.limit.MaxUSD > 0 {
		pct = (t.lastWarnReport.TotalUSD / t.limit.MaxUSD) * 100
	}
	return fmt.Sprintf(
		"[Budget warning: $%.2f of $%.2f used (%.0f%%). Wrap up: finish the current change, do not start new work, do not spawn additional sub-agents.]",
		t.lastWarnReport.TotalUSD, t.limit.MaxUSD, pct,
	), true
}
