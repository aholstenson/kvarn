package scheduler_test

import (
	"context"
	"time"

	"github.com/aholstenson/kvarn/internal/orchestrator/scheduler"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("TryAcquire", func() {
	var s *scheduler.Scheduler

	BeforeEach(func() {
		s = scheduler.New(scheduler.Options{Total: cap4(), CPUOvercommit: 1.0})
	})

	It("admits a request that fits right now", func() {
		lease, err := s.TryAcquire(req(2000, 4, 16))
		Expect(err).NotTo(HaveOccurred())
		Expect(lease).NotTo(BeNil())

		used, _, qlen := s.Snapshot()
		Expect(used.CPUMillis).To(Equal(uint64(2000)))
		Expect(qlen).To(Equal(0))

		lease.Release()
		used, _, _ = s.Snapshot()
		Expect(used.CPUMillis).To(Equal(uint64(0)))
	})

	It("returns ErrWouldBlock instead of queueing when the pool is full", func() {
		held, err := s.TryAcquire(req(1000, 8, 16))
		Expect(err).NotTo(HaveOccurred())

		_, err = s.TryAcquire(req(1000, 4, 16))
		Expect(err).To(MatchError(scheduler.ErrWouldBlock))

		// The refused request left no waiter behind: a queued entry would
		// block backfill and would be admitted later with nobody waiting for
		// the lease.
		_, _, qlen := s.Snapshot()
		Expect(qlen).To(Equal(0))

		held.Release()
	})

	It("admits again once capacity frees up", func() {
		held, err := s.TryAcquire(req(1000, 8, 16))
		Expect(err).NotTo(HaveOccurred())
		_, err = s.TryAcquire(req(1000, 4, 16))
		Expect(err).To(MatchError(scheduler.ErrWouldBlock))

		held.Release()

		lease, err := s.TryAcquire(req(1000, 4, 16))
		Expect(err).NotTo(HaveOccurred())
		lease.Release()
	})

	It("rejects a request larger than the pool with ErrTooLarge", func() {
		_, err := s.TryAcquire(req(8000, 4, 16))
		Expect(err).To(MatchError(scheduler.ErrTooLarge))
	})

	It("admits everything on an unbounded scheduler", func() {
		unbounded := scheduler.NewUnbounded()
		for range 5 {
			lease, err := unbounded.TryAcquire(req(4000, 8, 40))
			Expect(err).NotTo(HaveOccurred())
			Expect(lease).NotTo(BeNil())
		}
	})

	It("does not disturb requests already waiting in the queue", func() {
		held, err := s.Acquire(context.Background(), req(1000, 8, 16))
		Expect(err).NotTo(HaveOccurred())

		waiting := make(chan scheduler.Lease, 1)
		go func() {
			defer GinkgoRecover()
			l, err := s.Acquire(context.Background(), req(1000, 4, 16))
			Expect(err).NotTo(HaveOccurred())
			waiting <- l
		}()
		Eventually(func() int { _, _, qlen := s.Snapshot(); return qlen }).Should(Equal(1))

		_, err = s.TryAcquire(req(1000, 4, 16))
		Expect(err).To(MatchError(scheduler.ErrWouldBlock))

		// The queued Acquire is still there and still gets its turn.
		_, _, qlen := s.Snapshot()
		Expect(qlen).To(Equal(1))
		held.Release()
		Eventually(waiting).Should(Receive())
	})

	It("respects a policy that reserves capacity for the head of the queue", func() {
		clock := newClock()
		s = scheduler.New(scheduler.Options{
			Total:         cap4(),
			CPUOvercommit: 1.0,
			Policy:        scheduler.Backfill{Grace: time.Minute},
			Now:           clock.Now,
		})

		held, err := s.Acquire(context.Background(), req(1000, 6, 16))
		Expect(err).NotTo(HaveOccurred())

		// A large request waits; it cannot fit alongside what is held.
		waiting := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			l, err := s.Acquire(context.Background(), req(1000, 8, 16))
			Expect(err).NotTo(HaveOccurred())
			close(waiting)
			l.Release()
		}()
		Eventually(func() int { _, _, qlen := s.Snapshot(); return qlen }).Should(Equal(1))

		// Past the grace window the head holds the line, so a small request
		// that would otherwise fit is refused rather than backfilled ahead.
		clock.advance(2 * time.Minute)
		_, err = s.TryAcquire(req(500, 1, 4))
		Expect(err).To(MatchError(scheduler.ErrWouldBlock))

		held.Release()
		Eventually(waiting).Should(BeClosed())
	})
})
