package scheduler_test

import (
	"context"
	"time"

	"github.com/aholstenson/kvarn/internal/orchestrator/scheduler"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// capped builds a scheduler with the default FIFO order behind the caps, which
// is how the orchestrator wires it.
func capped() *scheduler.Scheduler {
	return scheduler.New(scheduler.Options{
		Total:         cap4(),
		CPUOvercommit: 1.0,
		Policy:        scheduler.Capped{Inner: scheduler.FIFO{}},
	})
}

// limited is a 1 vCPU / 1 GiB / 1 GiB request from one tenant, so a pool of
// 4 vCPU is never the thing that stops it.
func limited(project, keyID string, projLimits, keyLimits scheduler.Limits) scheduler.Request {
	r := req(1000, 1, 1)
	r.Tenant = scheduler.Tenant{Project: project, KeyID: keyID}
	r.ProjectLimits = projLimits
	r.KeyLimits = keyLimits
	return r
}

// acquireAsync starts an Acquire that unwinds when the spec's context is
// cancelled, reporting the lease on the returned channel if it is admitted.
func acquireAsync(ctx context.Context, s *scheduler.Scheduler, r scheduler.Request) chan scheduler.Lease {
	out := make(chan scheduler.Lease, 1)
	go func() {
		defer GinkgoRecover()
		defer close(out)
		if lease, err := s.Acquire(ctx, r); err == nil {
			out <- lease
		}
	}()
	return out
}

var _ = Describe("Capped", func() {
	var (
		s      *scheduler.Scheduler
		ctx    context.Context
		cancel context.CancelFunc
	)

	oneJob := scheduler.Limits{MaxJobs: 1}
	twoJobs := scheduler.Limits{MaxJobs: 2}
	noLimit := scheduler.Limits{}

	BeforeEach(func() {
		s = capped()
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
	})

	It("behaves exactly like the inner policy when nothing is capped", func() {
		hold, err := s.Acquire(ctx, limited("alpha", "k1", noLimit, noLimit))
		Expect(err).NotTo(HaveOccurred())
		defer hold.Release()

		// Four more of the same fill the 4 vCPU pool, the fifth waits: the
		// pool, not a cap, is what stops it.
		for range 3 {
			_, err := s.Acquire(ctx, limited("alpha", "k1", noLimit, noLimit))
			Expect(err).NotTo(HaveOccurred())
		}
		waiting := acquireAsync(ctx, s, limited("alpha", "k1", noLimit, noLimit))
		Consistently(waiting, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("holds a project to its job cap", func() {
		hold, err := s.Acquire(ctx, limited("alpha", "k1", oneJob, noLimit))
		Expect(err).NotTo(HaveOccurred())

		second := acquireAsync(ctx, s, limited("alpha", "k1", oneJob, noLimit))
		Consistently(second, 50*time.Millisecond).ShouldNot(Receive())

		// The cap is the only thing holding it: the pool is nearly empty.
		_, free, _ := s.Snapshot()
		Expect(free.CPUMillis).To(Equal(uint64(3000)))

		hold.Release()
		Eventually(second).Should(Receive())
	})

	It("lets another tenant past a waiter its own cap is holding back", func() {
		hold, err := s.Acquire(ctx, limited("alpha", "k1", oneJob, noLimit))
		Expect(err).NotTo(HaveOccurred())
		defer hold.Release()

		blocked := acquireAsync(ctx, s, limited("alpha", "k1", oneJob, noLimit))
		Eventually(func() int { _, _, q := s.Snapshot(); return q }).Should(Equal(1))

		// beta arrives second and is admitted first. Under plain FIFO the
		// capped alpha waiter at the head would have stalled it too, turning
		// a per-project cap into a host-wide one.
		lease, err := s.Acquire(ctx, limited("beta", "k2", oneJob, noLimit))
		Expect(err).NotTo(HaveOccurred())
		lease.Release()

		Consistently(blocked, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("keeps a capped waiter's place in line among its own tenant", func() {
		hold, err := s.Acquire(ctx, limited("alpha", "k1", oneJob, noLimit))
		Expect(err).NotTo(HaveOccurred())

		first := acquireAsync(ctx, s, limited("alpha", "k1", oneJob, noLimit))
		Eventually(func() int { _, _, q := s.Snapshot(); return q }).Should(Equal(1))
		second := acquireAsync(ctx, s, limited("alpha", "k1", oneJob, noLimit))
		Eventually(func() int { _, _, q := s.Snapshot(); return q }).Should(Equal(2))

		hold.Release()
		Eventually(first).Should(Receive())
		Consistently(second, 50*time.Millisecond).ShouldNot(Receive())
	})

	It("caps a project's total capacity as well as its job count", func() {
		twoCPU := scheduler.Limits{Max: scheduler.Capacity{CPUMillis: 2000}}

		a, err := s.Acquire(ctx, limited("alpha", "k1", twoCPU, noLimit))
		Expect(err).NotTo(HaveOccurred())
		b, err := s.Acquire(ctx, limited("alpha", "k1", twoCPU, noLimit))
		Expect(err).NotTo(HaveOccurred())
		Expect(b).NotTo(BeNil())

		third := acquireAsync(ctx, s, limited("alpha", "k1", twoCPU, noLimit))
		Consistently(third, 50*time.Millisecond).ShouldNot(Receive())

		a.Release()
		Eventually(third).Should(Receive())
	})

	It("caps a key across every project it drives", func() {
		a, err := s.Acquire(ctx, limited("alpha", "k1", noLimit, twoJobs))
		Expect(err).NotTo(HaveOccurred())
		_, err = s.Acquire(ctx, limited("beta", "k1", noLimit, twoJobs))
		Expect(err).NotTo(HaveOccurred())

		// A third project on the same key is still the same key.
		third := acquireAsync(ctx, s, limited("gamma", "k1", noLimit, twoJobs))
		Consistently(third, 50*time.Millisecond).ShouldNot(Receive())

		// Another key is unaffected.
		other, err := s.Acquire(ctx, limited("gamma", "k2", noLimit, twoJobs))
		Expect(err).NotTo(HaveOccurred())
		other.Release()

		a.Release()
		Eventually(third).Should(Receive())
	})

	It("does not cap a scope with no identifier", func() {
		// Auth disabled: every job carries an empty key. A per-key cap must not
		// collapse into a host-wide one.
		for range 3 {
			_, err := s.Acquire(ctx, limited("alpha", "", noLimit, oneJob))
			Expect(err).NotTo(HaveOccurred())
		}

		usage := s.TenantUsage()
		Expect(usage[scheduler.Tenant{Project: "alpha"}].Jobs).To(Equal(3))
	})

	Describe("Precheck", func() {
		It("rejects a request larger than its own project cap", func() {
			r := limited("alpha", "k1", scheduler.Limits{
				Max: scheduler.Capacity{CPUMillis: 500},
			}, noLimit)

			_, err := s.Acquire(ctx, r)
			Expect(err).To(MatchError(scheduler.ErrExceedsLimit))
			Expect(err.Error()).To(ContainSubstring(`project "alpha"`))
			Expect(err.Error()).To(ContainSubstring("cpu"))

			// Rejected, not queued.
			_, _, qlen := s.Snapshot()
			Expect(qlen).To(Equal(0))
		})

		It("rejects a request larger than its own key cap", func() {
			r := limited("alpha", "k1", noLimit, scheduler.Limits{
				Max: scheduler.Capacity{MemBytes: 1},
			})

			_, err := s.Acquire(ctx, r)
			Expect(err).To(MatchError(scheduler.ErrExceedsLimit))
			Expect(err.Error()).To(ContainSubstring("API key"))
			Expect(err.Error()).To(ContainSubstring("memory"))
		})

		It("does not reject on a job cap, which only ever means wait", func() {
			_, err := s.Acquire(ctx, limited("alpha", "k1", oneJob, noLimit))
			Expect(err).NotTo(HaveOccurred())
		})

		It("ignores a cap on a scope the request does not name", func() {
			// An empty project cannot be over its project cap, however small.
			r := limited("", "", scheduler.Limits{
				Max: scheduler.Capacity{CPUMillis: 1},
			}, scheduler.Limits{Max: scheduler.Capacity{CPUMillis: 1}})

			lease, err := s.Acquire(ctx, r)
			Expect(err).NotTo(HaveOccurred())
			lease.Release()
		})
	})
})
