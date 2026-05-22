package cost_test

import (
	"context"
	"errors"
	"io"
	"log/slog"

	llms "github.com/aholstenson/llms-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/agent/cost"
)

// stubCollector is a Collector that returns a fixed counter map. The tracker
// passes whatever Collector we give to the recorder; the recorder copies via
// GetCounters().
type stubCollector struct {
	counters map[string]int
}

func (s *stubCollector) Counter(name string) llms.Counter { return &noopCounter{} }
func (s *stubCollector) GetCounters() map[string]int      { return s.counters }

type noopCounter struct{}

func (n *noopCounter) Add(int) {}

// recordUsage shoves a fixed input/output token count for serviceName into
// tracker's recorder via the public llms-go MetricsRecorder interface.
func recordUsage(t *cost.Tracker, serviceName string, input, output, cached int) {
	t.Recorder().RecordSuccess(serviceName, &stubCollector{
		counters: map[string]int{
			"input_tokens":       input,
			"output_tokens":      output,
			"cached_read_tokens": cached,
		},
	})
}

var _ = Describe("Tracker", func() {
	var pricing *llms.PricingManager

	BeforeEach(func() {
		pricing = llms.NewPricingManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	})

	It("returns a zero report when no spend has been recorded", func() {
		t := cost.NewTracker(cost.TrackerOpts{Pricing: pricing})
		r := t.Snapshot()
		Expect(r.TotalUSD).To(BeZero())
		Expect(r.InputTokens).To(BeZero())
		Expect(r.PerModel).To(BeEmpty())
	})

	It("attributes tokens and USD per model", func() {
		t := cost.NewTracker(cost.TrackerOpts{Pricing: pricing})
		// Use a real model name so pricing resolves. Haiku 4.5 is cheap.
		recordUsage(t, "anthropic/claude-haiku-4-5", 1_000_000, 500_000, 0)
		recordUsage(t, "anthropic/claude-sonnet-4-6", 2_000_000, 1_000_000, 0)

		r := t.Snapshot()
		Expect(r.InputTokens).To(Equal(int64(3_000_000)))
		Expect(r.OutputTokens).To(Equal(int64(1_500_000)))
		Expect(r.PerModel).To(HaveLen(2))
		Expect(r.PerModel["anthropic/claude-haiku-4-5"].TotalUSD).To(BeNumerically(">", 0))
		Expect(r.PerModel["anthropic/claude-sonnet-4-6"].TotalUSD).To(
			BeNumerically(">", r.PerModel["anthropic/claude-haiku-4-5"].TotalUSD),
			"sonnet is pricier per token than haiku",
		)
		Expect(r.TotalUSD).To(BeNumerically("~",
			r.PerModel["anthropic/claude-haiku-4-5"].TotalUSD+
				r.PerModel["anthropic/claude-sonnet-4-6"].TotalUSD,
			1e-9))
	})

	It("OverBudget flips at the configured limit", func() {
		t := cost.NewTracker(cost.TrackerOpts{
			Pricing: pricing,
			Limit:   cost.Limit{MaxUSD: 0.01},
		})
		Expect(t.OverBudget()).To(BeFalse())
		// Sonnet input is $3/M, so 5M input ≈ $15.
		recordUsage(t, "anthropic/claude-sonnet-4-6", 5_000_000, 0, 0)
		Expect(t.OverBudget()).To(BeTrue())
	})

	It("OverBudget returns false when no limit is set", func() {
		t := cost.NewTracker(cost.TrackerOpts{Pricing: pricing})
		recordUsage(t, "anthropic/claude-sonnet-4-6", 5_000_000, 0, 0)
		Expect(t.OverBudget()).To(BeFalse())
	})

	It("fires warning once when warn threshold is crossed", func() {
		var warns int
		var warnReport cost.Report
		t := cost.NewTracker(cost.TrackerOpts{
			Pricing: pricing,
			Limit:   cost.Limit{MaxUSD: 100, WarnFraction: 0.5},
			OnWarning: func(r cost.Report) {
				warns++
				warnReport = r
			},
		})

		// Under threshold.
		recordUsage(t, "anthropic/claude-haiku-4-5", 1_000, 0, 0)
		t.CheckBudget()
		Expect(warns).To(BeZero())

		// Cross threshold ($50 limit = $25 warn). Sonnet input $3/M, so
		// 20M input ≈ $60 > $50.
		recordUsage(t, "anthropic/claude-sonnet-4-6", 20_000_000, 0, 0)
		t.CheckBudget()
		Expect(warns).To(Equal(1))
		Expect(warnReport.TotalUSD).To(BeNumerically(">=", 50))

		// Additional checks do not re-fire.
		recordUsage(t, "anthropic/claude-sonnet-4-6", 1_000_000, 0, 0)
		t.CheckBudget()
		Expect(warns).To(Equal(1))
	})

	It("cancels ctx with ErrBudgetExceeded when hard limit is hit", func() {
		ctx, cancel := context.WithCancelCause(context.Background())
		var overCalled bool
		t := cost.NewTracker(cost.TrackerOpts{
			Pricing: pricing,
			Limit:   cost.Limit{MaxUSD: 1.00, WarnFraction: 0.8},
			Cancel:  cancel,
			OnOverBudget: func(cost.Report) {
				overCalled = true
			},
		})

		// Push over $1.00. Sonnet input is $3/M, so 1M input ≈ $3.
		recordUsage(t, "anthropic/claude-sonnet-4-6", 1_000_000, 0, 0)
		t.CheckBudget()

		Expect(overCalled).To(BeTrue())
		Expect(ctx.Err()).To(MatchError(context.Canceled))
		Expect(errors.Is(context.Cause(ctx), cost.ErrBudgetExceeded)).To(BeTrue())

		// Second crossing does not re-cancel.
		overCalled = false
		recordUsage(t, "anthropic/claude-sonnet-4-6", 1_000_000, 0, 0)
		t.CheckBudget()
		Expect(overCalled).To(BeFalse())
	})

	It("ConsumeWarning returns the note exactly once after warn fires", func() {
		t := cost.NewTracker(cost.TrackerOpts{
			Pricing: pricing,
			Limit:   cost.Limit{MaxUSD: 100, WarnFraction: 0.5},
		})

		// Before warn fires.
		_, ok := t.ConsumeWarning()
		Expect(ok).To(BeFalse())

		// Cross warn threshold.
		recordUsage(t, "anthropic/claude-sonnet-4-6", 20_000_000, 0, 0)
		t.CheckBudget()

		note, ok := t.ConsumeWarning()
		Expect(ok).To(BeTrue())
		Expect(note).To(ContainSubstring("Budget warning"))
		Expect(note).To(ContainSubstring("Wrap up"))

		// Second call returns nothing.
		_, ok = t.ConsumeWarning()
		Expect(ok).To(BeFalse())
	})

	It("Snapshot is safe to call without a pricing manager", func() {
		t := cost.NewTracker(cost.TrackerOpts{})
		recordUsage(t, "anthropic/claude-sonnet-4-6", 1_000, 500, 0)
		r := t.Snapshot()
		Expect(r.InputTokens).To(Equal(int64(1_000)))
		Expect(r.OutputTokens).To(Equal(int64(500)))
		Expect(r.TotalUSD).To(BeZero())
	})
})
