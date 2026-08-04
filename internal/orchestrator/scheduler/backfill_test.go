package scheduler_test

import (
	"context"
	"sync"
	"time"

	"github.com/aholstenson/kvarn/internal/orchestrator/scheduler"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// fakeClock lets a spec age waiters past the grace window without sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Now()}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

var _ = Describe("Backfill", func() {
	var (
		clock  *fakeClock
		s      *scheduler.Scheduler
		ctx    context.Context
		cancel context.CancelFunc
	)

	const grace = time.Minute

	BeforeEach(func() {
		clock = newClock()
		s = scheduler.New(scheduler.Options{
			Total:         cap4(),
			CPUOvercommit: 1.0,
			Policy:        scheduler.Backfill{Grace: grace},
			Now:           clock.Now,
		})
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
	})

	queueLen := func() int { _, _, q := s.Snapshot(); return q }

	It("admits a small job past a large one that does not fit", func() {
		hold, err := s.Acquire(ctx, req(3000, 6, 30))
		Expect(err).NotTo(HaveOccurred())
		defer hold.Release()

		big := acquireAsync(ctx, s, req(3000, 6, 30))
		Eventually(queueLen).Should(Equal(1))

		lease, err := s.Acquire(ctx, req(500, 1, 1))
		Expect(err).NotTo(HaveOccurred())
		lease.Release()

		Consistently(big, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("stops admitting past a waiter once it has waited out the grace", func() {
		hold, err := s.Acquire(ctx, req(3000, 6, 30))
		Expect(err).NotTo(HaveOccurred())
		defer hold.Release()

		big := acquireAsync(ctx, s, req(3000, 6, 30))
		Eventually(queueLen).Should(Equal(1))

		clock.advance(grace)

		// The same small request the previous spec admitted is now refused:
		// the big one holds the line.
		small, cancelSmall := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancelSmall()
		_, err = s.Acquire(small, req(500, 1, 1))
		Expect(err).To(MatchError(context.DeadlineExceeded))

		Consistently(big, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("admits the waiter that held the line as soon as capacity frees", func() {
		hold, err := s.Acquire(ctx, req(3000, 6, 30))
		Expect(err).NotTo(HaveOccurred())

		big := acquireAsync(ctx, s, req(3000, 6, 30))
		Eventually(queueLen).Should(Equal(1))
		clock.advance(grace)

		hold.Release()
		Eventually(big).Should(Receive())
	})

	It("ages every waiter, not only the head", func() {
		hold, err := s.Acquire(ctx, req(3000, 6, 30))
		Expect(err).NotTo(HaveOccurred())
		defer hold.Release()

		// Two large waiters. Both age past the grace, so the second holds the
		// line just as the first does — a run of large jobs cannot starve the
		// one behind the head.
		first := acquireAsync(ctx, s, req(3000, 6, 30))
		Eventually(queueLen).Should(Equal(1))
		second := acquireAsync(ctx, s, req(2000, 4, 20))
		Eventually(queueLen).Should(Equal(2))

		clock.advance(grace)

		small, cancelSmall := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancelSmall()
		_, err = s.Acquire(small, req(500, 1, 1))
		Expect(err).To(MatchError(context.DeadlineExceeded))

		Consistently(first, 50*time.Millisecond).ShouldNot(Receive())
		Consistently(second, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("keeps skipping a young waiter that sits behind an aged one", func() {
		hold, err := s.Acquire(ctx, req(4000, 8, 40))
		Expect(err).NotTo(HaveOccurred())

		aged := acquireAsync(ctx, s, req(3000, 6, 30))
		Eventually(queueLen).Should(Equal(1))
		clock.advance(grace)

		// Arrives after the line is already held, so it waits even though it
		// would fit the capacity the release frees.
		young := acquireAsync(ctx, s, req(500, 1, 1))
		Eventually(queueLen).Should(Equal(2))

		hold.Release()
		Eventually(aged).Should(Receive())
		Eventually(young).Should(Receive(), "and follows once the line is released")
	})

	It("is strict FIFO at a zero grace", func() {
		fifo := scheduler.New(scheduler.Options{
			Total:         cap4(),
			CPUOvercommit: 1.0,
			Policy:        scheduler.Backfill{},
			Now:           clock.Now,
		})

		hold, err := fifo.Acquire(ctx, req(3000, 6, 30))
		Expect(err).NotTo(HaveOccurred())
		defer hold.Release()

		big := acquireAsync(ctx, fifo, req(3000, 6, 30))
		Eventually(func() int { _, _, q := fifo.Snapshot(); return q }).Should(Equal(1))

		small, cancelSmall := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancelSmall()
		_, err = fifo.Acquire(small, req(500, 1, 1))
		Expect(err).To(MatchError(context.DeadlineExceeded))
		Consistently(big, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("does not let a capped waiter hold the line", func() {
		// A capped waiter can only be freed by its own tenant finishing a job,
		// so reserving on its behalf would stall the host on an event that has
		// nothing to do with capacity.
		s := scheduler.New(scheduler.Options{
			Total:         cap4(),
			CPUOvercommit: 1.0,
			Policy: scheduler.Capped{
				Inner: scheduler.Backfill{Grace: grace},
			},
			Now: clock.Now,
		})
		oneJob := scheduler.Limits{MaxJobs: 1}

		hold, err := s.Acquire(ctx, limited("alpha", "k1", oneJob, scheduler.Limits{}))
		Expect(err).NotTo(HaveOccurred())
		defer hold.Release()

		blocked := acquireAsync(ctx, s, limited("alpha", "k1", oneJob, scheduler.Limits{}))
		Eventually(func() int { _, _, q := s.Snapshot(); return q }).Should(Equal(1))
		clock.advance(grace)

		lease, err := s.Acquire(ctx, limited("beta", "k2", oneJob, scheduler.Limits{}))
		Expect(err).NotTo(HaveOccurred())
		lease.Release()

		Consistently(blocked, 50*time.Millisecond).ShouldNot(Receive())
	})
})
