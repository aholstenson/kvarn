package scheduler_test

import (
	"context"
	"time"

	"github.com/aholstenson/kvarn/internal/orchestrator/scheduler"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Queue bound", func() {
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)

	BeforeEach(func() {
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
	})

	bounded := func(max int, policy scheduler.Policy) *scheduler.Scheduler {
		return scheduler.New(scheduler.Options{
			Total:         cap4(),
			CPUOvercommit: 1.0,
			MaxQueue:      max,
			Policy:        policy,
		})
	}

	It("refuses a request that would exceed the bound", func() {
		s := bounded(2, scheduler.FIFO{})
		hold, err := s.Acquire(ctx, req(4000, 8, 40))
		Expect(err).NotTo(HaveOccurred())
		defer hold.Release()

		first := acquireAsync(ctx, s, req(1000, 1, 1))
		Eventually(func() int { _, _, q := s.Snapshot(); return q }).Should(Equal(1))
		second := acquireAsync(ctx, s, req(1000, 1, 1))
		Eventually(func() int { _, _, q := s.Snapshot(); return q }).Should(Equal(2))

		_, err = s.Acquire(ctx, req(1000, 1, 1))
		Expect(err).To(MatchError(scheduler.ErrQueueFull))

		// The refused request left nothing behind.
		_, _, qlen := s.Snapshot()
		Expect(qlen).To(Equal(2))
		Consistently(first, 50*time.Millisecond).ShouldNot(Receive())
		Consistently(second, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("still admits a request the pool can run right now", func() {
		// A full queue of jobs that do not fit must not turn away one that
		// does: a queue bound exists to stop waiting work piling up, not to
		// stop work from running.
		s := bounded(1, scheduler.Backfill{Grace: time.Hour})

		hold, err := s.Acquire(ctx, req(3000, 6, 30))
		Expect(err).NotTo(HaveOccurred())
		defer hold.Release()

		big := acquireAsync(ctx, s, req(3000, 6, 30))
		Eventually(func() int { _, _, q := s.Snapshot(); return q }).Should(Equal(1))
		Expect(s.QueueFull()).To(BeTrue())

		lease, err := s.Acquire(ctx, req(500, 1, 1))
		Expect(err).NotTo(HaveOccurred())
		lease.Release()

		Consistently(big, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("takes a new waiter once one leaves the queue", func() {
		s := bounded(1, scheduler.FIFO{})
		hold, err := s.Acquire(ctx, req(4000, 8, 40))
		Expect(err).NotTo(HaveOccurred())
		defer hold.Release()

		queuedCtx, cancelQueued := context.WithCancel(ctx)
		queued := acquireAsync(queuedCtx, s, req(1000, 1, 1))
		Eventually(func() int { _, _, q := s.Snapshot(); return q }).Should(Equal(1))

		_, err = s.Acquire(ctx, req(1000, 1, 1))
		Expect(err).To(MatchError(scheduler.ErrQueueFull))

		cancelQueued()
		Eventually(queued).Should(BeClosed())
		Eventually(func() int { _, _, q := s.Snapshot(); return q }).Should(Equal(0))
		Expect(s.QueueFull()).To(BeFalse())

		accepted := acquireAsync(ctx, s, req(1000, 1, 1))
		Eventually(func() int { _, _, q := s.Snapshot(); return q }).Should(Equal(1))
		Consistently(accepted, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("is unbounded at zero", func() {
		s := bounded(0, scheduler.FIFO{})
		hold, err := s.Acquire(ctx, req(4000, 8, 40))
		Expect(err).NotTo(HaveOccurred())
		defer hold.Release()

		for range 20 {
			acquireAsync(ctx, s, req(1000, 1, 1))
		}
		Eventually(func() int { _, _, q := s.Snapshot(); return q }).Should(Equal(20))
		Expect(s.QueueFull()).To(BeFalse())
	})

	It("reports no bound for the unbounded scheduler", func() {
		Expect(scheduler.NewUnbounded().QueueFull()).To(BeFalse())
	})
})
