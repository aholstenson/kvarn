package scheduler_test

import (
	"context"
	"time"

	"github.com/aholstenson/kvarn/internal/orchestrator/scheduler"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// prioritized is a 1 vCPU request from one project at a given priority.
func prioritized(project string, priority int) scheduler.Request {
	r := req(1000, 1, 1)
	r.Tenant = scheduler.Tenant{Project: project}
	r.Priority = priority
	return r
}

var _ = Describe("Fair", func() {
	const ageStep = 5 * time.Minute

	var (
		clock  *fakeClock
		ctx    context.Context
		cancel context.CancelFunc
	)

	BeforeEach(func() {
		clock = newClock()
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
	})

	newFair := func(ageStep time.Duration) *scheduler.Scheduler {
		return scheduler.New(scheduler.Options{
			Total:         cap4(),
			CPUOvercommit: 1.0,
			Policy:        scheduler.Fair{AgeStep: ageStep},
			Now:           clock.Now,
		})
	}

	// fill occupies the whole pool so everything after it queues, and returns
	// the lease that releases exactly one slot at a time.
	fill := func(s *scheduler.Scheduler, project string) []scheduler.Lease {
		var out []scheduler.Lease
		for range 4 {
			lease, err := s.Acquire(ctx, prioritized(project, 0))
			Expect(err).NotTo(HaveOccurred())
			out = append(out, lease)
		}
		return out
	}

	queueLenOf := func(s *scheduler.Scheduler) func() int {
		return func() int { _, _, q := s.Snapshot(); return q }
	}

	It("serves the higher priority first regardless of arrival", func() {
		s := newFair(ageStep)
		held := fill(s, "filler")

		low := acquireAsync(ctx, s, prioritized("alpha", 0))
		Eventually(queueLenOf(s)).Should(Equal(1))
		high := acquireAsync(ctx, s, prioritized("beta", 5))
		Eventually(queueLenOf(s)).Should(Equal(2))

		held[0].Release()
		Eventually(high).Should(Receive())
		Consistently(low, 50*time.Millisecond).ShouldNot(Receive())

		held[1].Release()
		Eventually(low).Should(Receive())
	})

	It("lets a waiting job age into the priority it was passed over for", func() {
		s := newFair(ageStep)
		held := fill(s, "filler")

		low := acquireAsync(ctx, s, prioritized("alpha", 0))
		Eventually(queueLenOf(s)).Should(Equal(1))
		high := acquireAsync(ctx, s, prioritized("beta", 1))
		Eventually(queueLenOf(s)).Should(Equal(2))

		// One step of aging lifts the low job to the high one's level, where
		// the arrival tiebreak — it queued first — puts it in front.
		clock.advance(ageStep)

		held[0].Release()
		Eventually(low).Should(Receive())
		Consistently(high, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("holds the priority order between waiters that have aged equally", func() {
		s := newFair(ageStep)
		held := fill(s, "filler")

		// Both queue at the same moment and age at the same rate, so however
		// long they wait the gap between them is unchanged: aging exists to
		// rescue a job that is being *passed over*, not to churn an order that
		// is already settled.
		high := acquireAsync(ctx, s, prioritized("beta", 1))
		Eventually(queueLenOf(s)).Should(Equal(1))
		low := acquireAsync(ctx, s, prioritized("alpha", 0))
		Eventually(queueLenOf(s)).Should(Equal(2))

		clock.advance(10 * ageStep)

		held[0].Release()
		Eventually(high).Should(Receive())
		Consistently(low, 50*time.Millisecond).ShouldNot(Receive())

		held[1].Release()
		Eventually(low).Should(Receive())
	})

	It("keeps priority absolute when aging is disabled", func() {
		s := newFair(0)
		held := fill(s, "filler")

		low := acquireAsync(ctx, s, prioritized("alpha", 0))
		Eventually(queueLenOf(s)).Should(Equal(1))
		high := acquireAsync(ctx, s, prioritized("beta", 1))
		Eventually(queueLenOf(s)).Should(Equal(2))

		clock.advance(100 * ageStep)

		held[0].Release()
		Eventually(high).Should(Receive())
		Consistently(low, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("serves the project holding less of the host first", func() {
		s := newFair(ageStep)

		// hog holds 3 of the 4 vCPU; idler holds none.
		var held []scheduler.Lease
		for range 3 {
			lease, err := s.Acquire(ctx, prioritized("hog", 0))
			Expect(err).NotTo(HaveOccurred())
			held = append(held, lease)
		}
		last, err := s.Acquire(ctx, prioritized("filler", 0))
		Expect(err).NotTo(HaveOccurred())
		held = append(held, last)

		// The hog's next job queues first, so arrival order alone would serve
		// it first. Dominant share is what puts the idle project ahead.
		hogNext := acquireAsync(ctx, s, prioritized("hog", 0))
		Eventually(queueLenOf(s)).Should(Equal(1))
		idler := acquireAsync(ctx, s, prioritized("idler", 0))
		Eventually(queueLenOf(s)).Should(Equal(2))

		held[0].Release()
		Eventually(idler).Should(Receive())
		Consistently(hogNext, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("falls back to arrival order when every waiter ties", func() {
		s := newFair(ageStep)
		held := fill(s, "alpha")

		first := acquireAsync(ctx, s, prioritized("alpha", 0))
		Eventually(queueLenOf(s)).Should(Equal(1))
		second := acquireAsync(ctx, s, prioritized("alpha", 0))
		Eventually(queueLenOf(s)).Should(Equal(2))

		held[0].Release()
		Eventually(first).Should(Receive())
		Consistently(second, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("does not let age alone reorder a queue nobody prioritized", func() {
		// With every priority at zero the ceiling is zero, so no aging applies
		// and dominant share still decides. Without the clamp the older waiter
		// would climb into a higher bucket and fairness would stop mattering.
		s := newFair(ageStep)

		var held []scheduler.Lease
		for range 3 {
			lease, err := s.Acquire(ctx, prioritized("hog", 0))
			Expect(err).NotTo(HaveOccurred())
			held = append(held, lease)
		}
		last, err := s.Acquire(ctx, prioritized("filler", 0))
		Expect(err).NotTo(HaveOccurred())
		held = append(held, last)

		hogNext := acquireAsync(ctx, s, prioritized("hog", 0))
		Eventually(queueLenOf(s)).Should(Equal(1))
		clock.advance(100 * ageStep)
		idler := acquireAsync(ctx, s, prioritized("idler", 0))
		Eventually(queueLenOf(s)).Should(Equal(2))

		held[0].Release()
		Eventually(idler).Should(Receive())
		Consistently(hogNext, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("composes with Backfill so the most deserving waiter holds the line", func() {
		s := scheduler.New(scheduler.Options{
			Total:         cap4(),
			CPUOvercommit: 1.0,
			Policy: scheduler.Fair{
				AgeStep: ageStep,
				Inner:   scheduler.Backfill{Grace: time.Minute},
			},
			Now: clock.Now,
		})

		hold, err := s.Acquire(ctx, req(3000, 6, 30))
		Expect(err).NotTo(HaveOccurred())
		defer hold.Release()

		// A large, high-priority waiter that cannot fit, and a small one that
		// could. While the large one is young the small one backfills past it.
		big := req(3000, 6, 30)
		big.Priority = 5
		bigW := acquireAsync(ctx, s, big)
		Eventually(queueLenOf(s)).Should(Equal(1))

		lease, err := s.Acquire(ctx, req(500, 1, 1))
		Expect(err).NotTo(HaveOccurred())
		lease.Release()

		// Once it ages past the grace it holds the line, priority and all.
		clock.advance(time.Minute)
		small, cancelSmall := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancelSmall()
		_, err = s.Acquire(small, req(500, 1, 1))
		Expect(err).To(MatchError(context.DeadlineExceeded))
		Consistently(bigW, 50*time.Millisecond).ShouldNot(Receive())
	})
})
