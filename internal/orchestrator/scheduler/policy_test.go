package scheduler_test

import (
	"context"
	"time"

	"github.com/aholstenson/kvarn/internal/orchestrator/scheduler"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// policyFunc adapts a function to the Policy interface.
type policyFunc func(scheduler.State) int

func (f policyFunc) Next(st scheduler.State) int { return f(st) }

// firstFit is the smallest policy that is not FIFO: it admits any waiter that
// fits, skipping over ones that do not. It stands in here for the backfilling
// policies the seam exists to allow.
var firstFit = policyFunc(func(st scheduler.State) int {
	for i, w := range st.Queue {
		if st.Free.Fits(w.Request) {
			return i
		}
	}
	return -1
})

func tenantReq(project string, cpu, mem, disk uint64) scheduler.Request {
	r := req(cpu, mem, disk)
	r.Tenant = scheduler.Tenant{Project: project}
	return r
}

var _ = Describe("Policy", func() {
	// A pool of 4 vCPU / 8 GiB / 40 GiB, per cap4.
	newWith := func(p scheduler.Policy) *scheduler.Scheduler {
		return scheduler.New(scheduler.Options{Total: cap4(), CPUOvercommit: 1.0, Policy: p})
	}

	It("defaults to FIFO when none is configured", func() {
		s := scheduler.New(scheduler.Options{Total: cap4(), CPUOvercommit: 1.0})

		hold, err := s.Acquire(context.Background(), req(3000, 6, 30))
		Expect(err).NotTo(HaveOccurred())

		// A big request that cannot fit, then a small one that could.
		bigDone := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			lease, err := s.Acquire(context.Background(), req(3000, 6, 30))
			Expect(err).NotTo(HaveOccurred())
			defer lease.Release()
			close(bigDone)
		}()
		Eventually(func() int { _, _, q := s.Snapshot(); return q }).Should(Equal(1))

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err = s.Acquire(ctx, req(500, 1, 1))
		Expect(err).To(MatchError(context.DeadlineExceeded), "the small request must not overtake the head")

		hold.Release()
		Eventually(bigDone).Should(BeClosed())
	})

	It("lets a policy admit past a head that does not fit", func() {
		s := newWith(firstFit)

		hold, err := s.Acquire(context.Background(), req(3000, 6, 30))
		Expect(err).NotTo(HaveOccurred())
		defer hold.Release()

		// Cancelled before hold is released, so the waiter unwinds with the
		// spec instead of being admitted as it tears down.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		blocked := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(blocked)
			if lease, err := s.Acquire(ctx, req(3000, 6, 30)); err == nil {
				lease.Release()
			}
		}()
		Eventually(func() int { _, _, q := s.Snapshot(); return q }).Should(Equal(1))

		// Same request FIFO refused above, admitted here because the policy is
		// free to look past the head.
		lease, err := s.Acquire(context.Background(), req(500, 1, 1))
		Expect(err).NotTo(HaveOccurred())
		lease.Release()

		Consistently(blocked, 50*time.Millisecond).ShouldNot(BeClosed(), "the head still does not fit")
	})

	It("shows the policy the free capacity left by each admission in a drain", func() {
		s := newWith(firstFit)

		hold, err := s.Acquire(context.Background(), req(4000, 8, 40))
		Expect(err).NotTo(HaveOccurred())

		// Three waiters of 2 vCPU each against a 4 vCPU pool: a drain must stop
		// after two, which it can only do by re-reading free capacity.
		var leases = make(chan scheduler.Lease, 3)
		for range 3 {
			go func() {
				defer GinkgoRecover()
				lease, err := s.Acquire(context.Background(), req(2000, 2, 10))
				Expect(err).NotTo(HaveOccurred())
				leases <- lease
			}()
		}
		Eventually(func() int { _, _, q := s.Snapshot(); return q }).Should(Equal(3))

		hold.Release()
		Eventually(func() int { return len(leases) }).Should(Equal(2))
		Consistently(func() int { return len(leases) }, 50*time.Millisecond).Should(Equal(2))

		used, _, qlen := s.Snapshot()
		Expect(used.CPUMillis).To(Equal(uint64(4000)))
		Expect(qlen).To(Equal(1))

		(<-leases).Release()
		Eventually(func() int { return len(leases) }).Should(Equal(2))
	})

	It("ignores an index that does not fit rather than overdrawing the pool", func() {
		// A policy that always names the head, fitting or not.
		s := newWith(policyFunc(func(st scheduler.State) int {
			if len(st.Queue) == 0 {
				return -1
			}
			return 0
		}))

		hold, err := s.Acquire(context.Background(), req(3000, 6, 30))
		Expect(err).NotTo(HaveOccurred())

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err = s.Acquire(ctx, req(3000, 6, 30))
		Expect(err).To(MatchError(context.DeadlineExceeded))

		used, free, _ := s.Snapshot()
		Expect(used.CPUMillis).To(Equal(uint64(3000)))
		Expect(free.CPUMillis).To(Equal(uint64(1000)))

		hold.Release()
		used, _, _ = s.Snapshot()
		Expect(used.CPUMillis).To(Equal(uint64(0)))
	})

	It("ignores an out-of-range index", func() {
		s := newWith(policyFunc(func(scheduler.State) int { return 99 }))

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := s.Acquire(ctx, req(500, 1, 1))
		Expect(err).To(MatchError(context.DeadlineExceeded))

		used, _, qlen := s.Snapshot()
		Expect(used).To(Equal(scheduler.Capacity{}))
		Expect(qlen).To(Equal(0))
	})

	It("passes the pool and the waiters' age to the policy", func() {
		seen := make(chan scheduler.State, 4)
		s := newWith(policyFunc(func(st scheduler.State) int {
			// State is not retained; the queue slice is copied out first.
			cp := st
			cp.Queue = append([]scheduler.Waiting(nil), st.Queue...)
			select {
			case seen <- cp:
			default:
			}
			return scheduler.FIFO{}.Next(st)
		}))

		before := time.Now()
		lease, err := s.Acquire(context.Background(), tenantReq("alpha", 1000, 1, 1))
		Expect(err).NotTo(HaveOccurred())
		defer lease.Release()

		var st scheduler.State
		Expect(seen).To(Receive(&st))
		Expect(st.Total).To(Equal(cap4()))
		Expect(st.Free).To(Equal(cap4()), "nothing is charged before the policy chooses")
		Expect(st.Queue).To(HaveLen(1))
		Expect(st.Queue[0].Tenant.Project).To(Equal("alpha"))
		Expect(st.Queue[0].EnqueuedAt).To(BeTemporally(">=", before))
	})
})

var _ = Describe("Tenant accounting", func() {
	var s *scheduler.Scheduler

	BeforeEach(func() {
		s = scheduler.New(scheduler.Options{Total: cap4(), CPUOvercommit: 1.0})
	})

	alpha := scheduler.Tenant{Project: "alpha"}
	beta := scheduler.Tenant{Project: "beta"}

	It("sums a tenant's running jobs and drops it when the last one ends", func() {
		a1, err := s.Acquire(context.Background(), tenantReq("alpha", 1000, 1, 1))
		Expect(err).NotTo(HaveOccurred())
		a2, err := s.Acquire(context.Background(), tenantReq("alpha", 500, 1, 1))
		Expect(err).NotTo(HaveOccurred())
		b1, err := s.Acquire(context.Background(), tenantReq("beta", 1000, 1, 1))
		Expect(err).NotTo(HaveOccurred())

		usage := s.TenantUsage()
		Expect(usage).To(HaveLen(2))
		Expect(usage[alpha].Jobs).To(Equal(2))
		Expect(usage[alpha].CPUMillis).To(Equal(uint64(1500)))
		Expect(usage[beta].Jobs).To(Equal(1))

		a1.Release()
		Expect(s.TenantUsage()[alpha].Jobs).To(Equal(1))
		a2.Release()
		Expect(s.TenantUsage()).NotTo(HaveKey(alpha))
		Expect(s.TenantUsage()).To(HaveKey(beta))

		b1.Release()
		Expect(s.TenantUsage()).To(BeEmpty())
	})

	It("accounts requests naming no tenant under the zero value", func() {
		lease, err := s.Acquire(context.Background(), req(1000, 1, 1))
		Expect(err).NotTo(HaveOccurred())

		usage := s.TenantUsage()
		Expect(usage).To(HaveLen(1))
		Expect(usage[scheduler.Tenant{}].Jobs).To(Equal(1))

		lease.Release()
		Expect(s.TenantUsage()).To(BeEmpty())
	})

	It("does not leak a tenant's usage when a grant races cancellation", func() {
		hold, err := s.Acquire(context.Background(), tenantReq("alpha", 4000, 8, 40))
		Expect(err).NotTo(HaveOccurred())

		ctx, cancel := context.WithCancel(context.Background())
		acquired := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			lease, err := s.Acquire(ctx, tenantReq("beta", 1000, 1, 1))
			if err == nil {
				lease.Release()
			}
			acquired <- err
		}()
		Eventually(func() int { _, _, q := s.Snapshot(); return q }).Should(Equal(1))

		// Cancel and release together, so the waiter may be granted capacity
		// and abandon it.
		cancel()
		hold.Release()
		Eventually(acquired).Should(Receive())

		Eventually(s.TenantUsage).Should(BeEmpty())
		used, _, _ := s.Snapshot()
		Expect(used).To(Equal(scheduler.Capacity{}))
	})

	It("reports no tenant usage for the unbounded scheduler", func() {
		Expect(scheduler.NewUnbounded().TenantUsage()).To(BeNil())
	})
})
