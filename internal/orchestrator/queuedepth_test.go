package orchestrator

import (
	"context"

	"connectrpc.com/connect"
	"github.com/aholstenson/kvarn/internal/orchestrator/scheduler"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("checkQueueDepth", func() {
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)

	small := scheduler.Capacity{
		CPUMillis: 1000,
		MemBytes:  1024 * 1024 * 1024,
		DiskBytes: 1024 * 1024 * 1024,
	}
	one := scheduler.Request{CPUMillis: 1000, MemBytes: 1024 * 1024 * 1024, DiskBytes: 1024 * 1024 * 1024}

	BeforeEach(func() {
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
	})

	It("accepts while the queue has room", func() {
		svc := &Service{scheduler: scheduler.New(scheduler.Options{Total: small, MaxQueue: 1})}
		Expect(svc.checkQueueDepth(ctx, "alpha")).To(Succeed())
	})

	It("refuses with ResourceExhausted once the queue is full", func() {
		sched := scheduler.New(scheduler.Options{Total: small, MaxQueue: 1})
		svc := &Service{scheduler: sched}

		hold, err := sched.Acquire(ctx, one)
		Expect(err).NotTo(HaveOccurred())
		defer hold.Release()

		go func() {
			defer GinkgoRecover()
			if lease, err := sched.Acquire(ctx, one); err == nil {
				lease.Release()
			}
		}()
		Eventually(func() int { _, _, q := sched.Snapshot(); return q }).Should(Equal(1))

		err = svc.checkQueueDepth(ctx, "alpha")
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeResourceExhausted))
	})

	It("accepts against an unbounded scheduler", func() {
		svc := &Service{scheduler: scheduler.NewUnbounded()}
		Expect(svc.checkQueueDepth(ctx, "alpha")).To(Succeed())
	})
})

var _ = Describe("resolveCount", func() {
	It("takes the built-in default when neither input is set", func() {
		Expect(resolveCount(0, nil, 64)).To(Equal(64))
	})

	It("prefers the file over the default and the flag over both", func() {
		ten := 10
		Expect(resolveCount(0, &ten, 64)).To(Equal(10))
		Expect(resolveCount(5, &ten, 64)).To(Equal(5))
	})

	It("reads a negative as an explicit request for no bound", func() {
		// Zero already means unbounded in the field being set, so it cannot
		// also mean "unset" there — a negative is the only way to ask for
		// unbounded past a non-zero default.
		minusOne := -1
		Expect(resolveCount(-1, nil, 64)).To(Equal(0))
		Expect(resolveCount(0, &minusOne, 64)).To(Equal(0))
	})

	It("honors a zero set in the file", func() {
		zero := 0
		Expect(resolveCount(0, &zero, 64)).To(Equal(0))
	})
})
