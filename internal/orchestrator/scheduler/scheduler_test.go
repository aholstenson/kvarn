package scheduler_test

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aholstenson/kvarn/internal/orchestrator/scheduler"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func cap4() scheduler.Capacity {
	return scheduler.Capacity{
		CPUMillis: 4000,
		MemBytes:  8 * 1024 * 1024 * 1024,
		DiskBytes: 40 * 1024 * 1024 * 1024,
	}
}

func req(cpu uint64, mem uint64, disk uint64) scheduler.Request {
	return scheduler.Request{
		CPUMillis: cpu,
		MemBytes:  mem * 1024 * 1024 * 1024,
		DiskBytes: disk * 1024 * 1024 * 1024,
	}
}

var _ = Describe("Scheduler", func() {
	var s *scheduler.Scheduler

	BeforeEach(func() {
		s = scheduler.New(scheduler.Options{Total: cap4(), CPUOvercommit: 1.0})
	})

	It("admits a fitting request and reports granted capacity", func() {
		lease, err := s.Acquire(context.Background(), req(2000, 4, 16))
		Expect(err).NotTo(HaveOccurred())
		Expect(lease).NotTo(BeNil())
		g := lease.Granted()
		Expect(g.CPUMillis).To(Equal(uint64(2000)))
		Expect(g.MemBytes).To(Equal(uint64(4) * 1024 * 1024 * 1024))
		used, free, qlen := s.Snapshot()
		Expect(used.CPUMillis).To(Equal(uint64(2000)))
		Expect(free.CPUMillis).To(Equal(uint64(2000)))
		Expect(qlen).To(Equal(0))
		lease.Release()
		used, _, _ = s.Snapshot()
		Expect(used.CPUMillis).To(Equal(uint64(0)))
	})

	It("rejects requests exceeding total capacity", func() {
		_, err := s.Acquire(context.Background(), req(8000, 4, 16))
		Expect(err).To(MatchError(scheduler.ErrTooLarge))
		_, err = s.Acquire(context.Background(), req(1000, 100, 16))
		Expect(err).To(MatchError(scheduler.ErrTooLarge))
		_, err = s.Acquire(context.Background(), req(1000, 4, 100))
		Expect(err).To(MatchError(scheduler.ErrTooLarge))
	})

	It("blocks on full memory and admits after release", func() {
		l1, err := s.Acquire(context.Background(), req(1000, 8, 16))
		Expect(err).NotTo(HaveOccurred())

		// Second request needs memory but pool is full.
		acquired := make(chan scheduler.Lease, 1)
		go func() {
			l, _ := s.Acquire(context.Background(), req(1000, 4, 16))
			acquired <- l
		}()

		Consistently(acquired, 100*time.Millisecond).ShouldNot(Receive())
		l1.Release()
		Eventually(acquired).Should(Receive())
	})

	It("blocks on full disk and admits after release", func() {
		l1, err := s.Acquire(context.Background(), req(1000, 4, 40))
		Expect(err).NotTo(HaveOccurred())

		acquired := make(chan struct{})
		go func() {
			l, _ := s.Acquire(context.Background(), req(1000, 4, 16))
			defer l.Release()
			close(acquired)
		}()

		Consistently(acquired, 100*time.Millisecond).ShouldNot(BeClosed())
		l1.Release()
		Eventually(acquired).Should(BeClosed())
	})

	It("admits requests up to CPU overcommit when the pool is sized for it", func() {
		// 4 physical cores * 1.5 overcommit -> total 6000 millicpus.
		s := scheduler.New(scheduler.Options{
			Total: scheduler.Capacity{
				CPUMillis: 6000,
				MemBytes:  8 * 1024 * 1024 * 1024,
				DiskBytes: 40 * 1024 * 1024 * 1024,
			},
			CPUOvercommit: 1.5,
		})
		l, err := s.Acquire(context.Background(), req(6000, 4, 16))
		Expect(err).NotTo(HaveOccurred())
		l.Release()
	})

	It("enforces strict FIFO across the queue", func() {
		// Partially fill the pool so a small later waiter would fit if the
		// scheduler bypassed the head — strict FIFO must block it anyway.
		l1, _ := s.Acquire(context.Background(), req(2000, 2, 8))

		aAdmitted := make(chan scheduler.Lease, 1)
		bAdmitted := make(chan scheduler.Lease, 1)

		go func() {
			l, _ := s.Acquire(context.Background(), req(4000, 8, 40)) // full pool
			aAdmitted <- l
		}()
		Eventually(func() int {
			_, _, q := s.Snapshot()
			return q
		}).Should(Equal(1))

		go func() {
			l, _ := s.Acquire(context.Background(), req(1000, 1, 4)) // would fit alongside l1
			bAdmitted <- l
		}()
		Eventually(func() int {
			_, _, q := s.Snapshot()
			return q
		}).Should(Equal(2))

		// B fits in current free capacity but stays queued behind A.
		Consistently(bAdmitted, 100*time.Millisecond).ShouldNot(Receive())

		l1.Release()
		var la scheduler.Lease
		Eventually(aAdmitted).Should(Receive(&la))
		// A now holds the entire pool; B is still queued.
		Consistently(bAdmitted, 50*time.Millisecond).ShouldNot(Receive())

		la.Release()
		Eventually(bAdmitted).Should(Receive())
	})

	It("removes a cancelled waiter and lets subsequent waiters proceed", func() {
		l1, _ := s.Acquire(context.Background(), req(2000, 4, 16))
		l2, _ := s.Acquire(context.Background(), req(2000, 4, 16))

		ctx, cancel := context.WithCancel(context.Background())
		canceErr := make(chan error, 1)
		go func() {
			_, err := s.Acquire(ctx, req(2000, 4, 16))
			canceErr <- err
		}()
		Eventually(func() int {
			_, _, q := s.Snapshot()
			return q
		}).Should(Equal(1))

		nextAdmitted := make(chan scheduler.Lease, 1)
		go func() {
			l, _ := s.Acquire(context.Background(), req(2000, 4, 16))
			nextAdmitted <- l
		}()
		Eventually(func() int {
			_, _, q := s.Snapshot()
			return q
		}).Should(Equal(2))

		cancel()
		var err error
		Eventually(canceErr).Should(Receive(&err))
		Expect(err).To(MatchError(context.Canceled))

		// Cancelling the head should let the next waiter become the new head;
		// it still needs a release to admit.
		Consistently(nextAdmitted, 50*time.Millisecond).ShouldNot(Receive())
		l1.Release()
		Eventually(nextAdmitted).Should(Receive())
		l2.Release()
	})

	It("double-Release does not double-credit the pool", func() {
		l, err := s.Acquire(context.Background(), req(2000, 4, 16))
		Expect(err).NotTo(HaveOccurred())
		l.Release()
		l.Release()
		used, _, _ := s.Snapshot()
		Expect(used.CPUMillis).To(Equal(uint64(0)))

		// Pool should now accept a full-capacity request, confirming nothing
		// was over-credited or under-credited.
		l2, err := s.Acquire(context.Background(), req(4000, 8, 40))
		Expect(err).NotTo(HaveOccurred())
		l2.Release()
	})

	It("fires OnWait on enqueue and on every position change", func() {
		l1, _ := s.Acquire(context.Background(), req(2000, 4, 16))
		l2, _ := s.Acquire(context.Background(), req(2000, 4, 16))

		var (
			mu       sync.Mutex
			positionsA []int
			positionsB []int
		)
		makeCB := func(out *[]int) func(scheduler.WaitEvent) {
			return func(e scheduler.WaitEvent) {
				mu.Lock()
				*out = append(*out, e.Position)
				mu.Unlock()
			}
		}

		rA := req(2000, 4, 16)
		rA.OnWait = makeCB(&positionsA)
		rB := req(2000, 4, 16)
		rB.OnWait = makeCB(&positionsB)

		doneA := make(chan struct{})
		go func() {
			l, _ := s.Acquire(context.Background(), rA)
			defer l.Release()
			close(doneA)
		}()
		Eventually(func() int {
			mu.Lock()
			defer mu.Unlock()
			return len(positionsA)
		}).Should(Equal(1))

		doneB := make(chan struct{})
		go func() {
			l, _ := s.Acquire(context.Background(), rB)
			defer l.Release()
			close(doneB)
		}()
		Eventually(func() int {
			mu.Lock()
			defer mu.Unlock()
			return len(positionsB)
		}).Should(Equal(1))

		l1.Release()
		Eventually(doneA).Should(BeClosed())
		// B was position 2; after A admits and the head advances, B becomes 1.
		Eventually(func() []int {
			mu.Lock()
			defer mu.Unlock()
			return append([]int(nil), positionsB...)
		}).Should(Equal([]int{2, 1}))

		l2.Release()
		Eventually(doneB).Should(BeClosed())
	})

	It("stays consistent under concurrent acquire/release", func() {
		const workers = 32
		const iterations = 50
		var wg sync.WaitGroup
		var peak atomic.Uint64
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r := rand.New(rand.NewSource(1))
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(seed int64) {
				defer wg.Done()
				lr := rand.New(rand.NewSource(seed))
				for j := 0; j < iterations; j++ {
					cpu := uint64(500 + lr.Intn(2000))
					mem := uint64(1 + lr.Intn(3))
					disk := uint64(4 + lr.Intn(12))
					l, err := s.Acquire(ctx, req(cpu, mem, disk))
					if err != nil {
						return
					}
					used, _, _ := s.Snapshot()
					for {
						old := peak.Load()
						if used.CPUMillis <= old || peak.CompareAndSwap(old, used.CPUMillis) {
							break
						}
					}
					Expect(used.CPUMillis).To(BeNumerically("<=", uint64(4000)))
					Expect(used.MemBytes).To(BeNumerically("<=", uint64(8)*1024*1024*1024))
					Expect(used.DiskBytes).To(BeNumerically("<=", uint64(40)*1024*1024*1024))
					l.Release()
				}
			}(int64(i + 1))
			_ = r
		}
		wg.Wait()
		used, _, qlen := s.Snapshot()
		Expect(used).To(Equal(scheduler.Capacity{}))
		Expect(qlen).To(Equal(0))
	})
})

var _ = Describe("Unbounded scheduler", func() {
	It("admits any request immediately and noop releases", func() {
		s := scheduler.NewUnbounded()
		l, err := s.Acquire(context.Background(), req(99999, 99999, 99999))
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Granted().CPUMillis).To(Equal(uint64(99999)))
		l.Release()
		l.Release()
		used, free, qlen := s.Snapshot()
		Expect(used).To(Equal(scheduler.Capacity{}))
		Expect(free).To(Equal(scheduler.Capacity{}))
		Expect(qlen).To(Equal(0))
	})
})
