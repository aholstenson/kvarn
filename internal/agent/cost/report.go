package cost

import "errors"

// ErrBudgetExceeded is the canonical reason value used to cancel a per-job
// context when the cost limit is hit. Callers can match on it via
// errors.Is(ctx.Err()-cause, ErrBudgetExceeded).
var ErrBudgetExceeded = errors.New("cost: budget exceeded")

// Limit describes the cost budget for a job.
//
// MaxUSD is the hard cap. A non-positive value disables enforcement (no
// warning, no cancellation), in which case the tracker still records spend.
//
// WarnFraction is the fraction of MaxUSD (0..1) at which a soft warning fires.
// A non-positive value disables the warning.
type Limit struct {
	MaxUSD       float64
	WarnFraction float64
}

// ModelCost is the per-model contribution to a Report.
type ModelCost struct {
	ModelID      string
	InputTokens  int64
	OutputTokens int64
	CachedTokens int64
	TotalUSD     float64
}

// Report is a snapshot of accumulated LLM spend for a job.
type Report struct {
	InputTokens  int64
	OutputTokens int64
	CachedTokens int64
	TotalUSD     float64
	// PerModel is keyed by llms-go service name (the qualified model ID).
	PerModel map[string]ModelCost
}
