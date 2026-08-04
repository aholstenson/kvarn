package orchestrator

import (
	"context"
	"time"

	"connectrpc.com/connect"
	"github.com/aholstenson/kvarn/internal/session"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("checkBacklogDepth", func() {
	var (
		ctx    context.Context
		cancel context.CancelFunc
		mgr    session.Manager
	)

	// serviceWithBacklog builds a Service whose backlog is bounded at max,
	// without starting a dispatcher loop that would drain what the test queues.
	serviceWithBacklog := func(max int) *Service {
		svc := &Service{sessionMgr: mgr}
		svc.dispatcher = newDispatcher(svc, DispatchPolicy{MaxBacklog: max})
		return svc
	}

	queue := func(svc *Service, n int) {
		for range n {
			_, err := mgr.Create(ctx, session.CreateParams{ProjectName: "alpha", Prompt: "p", Mode: "auto"})
			Expect(err).NotTo(HaveOccurred())
		}
	}

	BeforeEach(func() {
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		mgr = session.NewManager(session.NewMemStore())
	})

	It("accepts while the backlog has room", func() {
		svc := serviceWithBacklog(2)
		queue(svc, 1)
		Expect(svc.checkBacklogDepth(ctx, "alpha")).To(Succeed())
	})

	It("refuses with ResourceExhausted once the backlog is full", func() {
		svc := serviceWithBacklog(2)
		queue(svc, 2)

		err := svc.checkBacklogDepth(ctx, "alpha")
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeResourceExhausted))
	})

	It("ignores sessions that have left the backlog", func() {
		svc := serviceWithBacklog(2)
		queue(svc, 2)

		// A dispatched job no longer occupies a backlog slot, so the room it
		// gives back is room a new submission can use.
		pending, err := mgr.ListPending(ctx, session.PendingQuery{Now: time.Now()})
		Expect(err).NotTo(HaveOccurred())
		won, err := mgr.TransitionPending(ctx, pending[0].ID, session.PendingTransition{State: session.StateQueued})
		Expect(err).NotTo(HaveOccurred())
		Expect(won).To(BeTrue())

		Expect(svc.checkBacklogDepth(ctx, "alpha")).To(Succeed())
	})

	It("accepts against an unbounded backlog", func() {
		svc := serviceWithBacklog(0)
		queue(svc, 5)
		Expect(svc.checkBacklogDepth(ctx, "alpha")).To(Succeed())
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
