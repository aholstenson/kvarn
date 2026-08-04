package scheduler_test

import (
	"context"
	"os"
	"time"

	"github.com/aholstenson/kvarn/internal/orchestrator/scheduler"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const gib = uint64(1024 * 1024 * 1024)

var _ = Describe("Host disk guard", func() {
	// A floor of 10 GiB against a pool with room for two 16 GiB jobs, so the
	// guard and the accounting pool can be made to disagree on purpose.
	guarded := func() *scheduler.Scheduler {
		return scheduler.New(scheduler.Options{
			Total:          cap4(),
			CPUOvercommit:  1.0,
			DiskOvercommit: 2.0,
			DiskPath:       "/nonexistent",
			DiskFloorBytes: 10 * gib,
		})
	}

	It("admits before any sample has been taken", func() {
		s := guarded()
		_, floor, measured, open := s.DiskGuard()
		Expect(floor).To(Equal(10 * gib))
		Expect(measured).To(BeFalse())
		Expect(open).To(BeTrue())

		lease, err := s.Acquire(context.Background(), req(1000, 1, 1))
		Expect(err).NotTo(HaveOccurred())
		lease.Release()
	})

	It("queues a request that fits the pool while free space is below the floor", func() {
		s := guarded()
		s.UpdateDiskAvailable(2 * gib)

		_, _, _, open := s.DiskGuard()
		Expect(open).To(BeFalse())

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := s.Acquire(ctx, req(1000, 1, 1))
		Expect(err).To(MatchError(context.DeadlineExceeded))

		// Nothing was charged for the request that never got in.
		used, _, qlen := s.Snapshot()
		Expect(used.CPUMillis).To(Equal(uint64(0)))
		Expect(qlen).To(Equal(0))
	})

	It("releases the queue when free space recovers", func() {
		s := guarded()
		s.UpdateDiskAvailable(2 * gib)

		admitted := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			lease, err := s.Acquire(context.Background(), req(1000, 1, 1))
			Expect(err).NotTo(HaveOccurred())
			defer lease.Release()
			close(admitted)
		}()

		Eventually(func() int {
			_, _, qlen := s.Snapshot()
			return qlen
		}).Should(Equal(1))
		Consistently(admitted, 50*time.Millisecond).ShouldNot(BeClosed())

		s.UpdateDiskAvailable(40 * gib)
		Eventually(admitted).Should(BeClosed())
	})

	It("tells a waiter that the host disk, not the pool, is what stopped it", func() {
		s := guarded()
		s.UpdateDiskAvailable(2 * gib)

		events := make(chan scheduler.WaitEvent, 8)
		r := req(1000, 1, 1)
		r.OnWait = func(e scheduler.WaitEvent) { events <- e }

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			defer GinkgoRecover()
			_, _ = s.Acquire(ctx, r)
		}()

		var e scheduler.WaitEvent
		Eventually(events).Should(Receive(&e))
		Expect(e.HostDiskLow).To(BeTrue())
		// The pool itself had room; that is exactly the confusion the flag exists
		// to clear up.
		Expect(e.Free.DiskBytes).To(BeNumerically(">", r.DiskBytes))
	})

	It("does not gate a scheduler configured without a floor", func() {
		s := scheduler.New(scheduler.Options{Total: cap4(), CPUOvercommit: 1.0})
		s.UpdateDiskAvailable(0)

		lease, err := s.Acquire(context.Background(), req(1000, 1, 1))
		Expect(err).NotTo(HaveOccurred())
		lease.Release()
	})

	It("does not gate the unbounded scheduler", func() {
		s := scheduler.NewUnbounded()
		s.UpdateDiskAvailable(0)
		_, _, _, open := s.DiskGuard()
		Expect(open).To(BeTrue())

		lease, err := s.Acquire(context.Background(), req(1000, 1, 1))
		Expect(err).NotTo(HaveOccurred())
		lease.Release()
	})

	Describe("WatchHostDisk", func() {
		It("returns immediately when no guard is configured", func() {
			s := scheduler.New(scheduler.Options{Total: cap4(), CPUOvercommit: 1.0})
			done := make(chan struct{})
			go func() {
				s.WatchHostDisk(context.Background(), time.Millisecond)
				close(done)
			}()
			Eventually(done).Should(BeClosed())
		})

		It("samples the real filesystem and stops with the context", func() {
			dir, err := os.MkdirTemp("", "kvarn-diskguard-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(dir)

			s := scheduler.New(scheduler.Options{
				Total:          cap4(),
				DiskPath:       dir,
				DiskFloorBytes: 1, // any real filesystem clears this
			})

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				s.WatchHostDisk(ctx, time.Millisecond)
				close(done)
			}()

			Eventually(func() bool {
				_, _, measured, _ := s.DiskGuard()
				return measured
			}).Should(BeTrue())
			avail, _, _, open := s.DiskGuard()
			Expect(avail).To(BeNumerically(">", 0))
			Expect(open).To(BeTrue())

			cancel()
			Eventually(done).Should(BeClosed())
		})
	})
})
