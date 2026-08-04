package orchestrator_test

import (
	"context"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/cmd/client"
	"github.com/aholstenson/kvarn/internal/config/apikey"
	"github.com/aholstenson/kvarn/internal/orchestrator"
	"github.com/aholstenson/kvarn/internal/orchestrator/auth"
	"github.com/aholstenson/kvarn/internal/session"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The queue-inspection specs need a backlog that stays put. A service built
// without a project store never dispatches — there would be nothing to resolve
// a job against — so entries written straight into the store remain pending for
// as long as the assertions need them.
var _ = Describe("Queue inspection", func() {
	var (
		ctx   context.Context
		store session.Store
		mgr   session.Manager
		base  time.Time
	)

	BeforeEach(func() {
		ctx = context.Background()
		store = session.NewMemStore()
		mgr = session.NewManager(store)
		base = time.Now().Add(-time.Hour)
	})

	// queued writes a backlog entry with a controlled arrival and priority.
	queued := func(id, project string, priority int, queuedAt time.Time) {
		Expect(store.CreateSession(ctx, &session.Session{
			ID:          id,
			ProjectName: project,
			Prompt:      "do " + id,
			Mode:        "review",
			State:       session.StatePending,
			Priority:    priority,
			CreatedAt:   queuedAt,
			UpdatedAt:   queuedAt,
			QueuedAt:    queuedAt,
		})).To(Succeed())
	}

	build := func(policy orchestrator.DispatchPolicy) *orchestrator.Service {
		svc := orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{
			SessionMgr: mgr,
			Dispatch:   policy,
		})
		DeferCleanup(func() { svc.Shutdown(context.Background()) })
		return svc
	}

	Describe("ListQueue", func() {
		It("returns the backlog in dispatch order with each entry's place", func() {
			queued("old-low", "alpha", 0, base)
			queued("new-high", "alpha", 5, base.Add(time.Minute))
			queued("old-high", "alpha", 5, base)

			svc := build(orchestrator.DispatchPolicy{})
			resp, err := svc.ListQueue(ctx, connect.NewRequest(&v1.ListQueueRequest{}))
			Expect(err).NotTo(HaveOccurred())

			ids := []string{}
			positions := []int32{}
			for _, e := range resp.Msg.Entries {
				ids = append(ids, e.Session.SessionId)
				positions = append(positions, e.Position)
			}
			Expect(ids).To(Equal([]string{"old-high", "new-high", "old-low"}))
			Expect(positions).To(Equal([]int32{1, 2, 3}))
			Expect(resp.Msg.Backlog).To(Equal(int32(3)))
		})

		It("reports the priority an aged entry is actually ordered by", func() {
			queued("waiting", "alpha", 0, base)
			queued("important", "alpha", 2, base.Add(59*time.Minute))

			svc := build(orchestrator.DispatchPolicy{PriorityAgeStep: 10 * time.Minute})
			resp, err := svc.ListQueue(ctx, connect.NewRequest(&v1.ListQueueRequest{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Entries).To(HaveLen(2))

			// Six age steps would put the waiting entry at 6; the clamp holds it
			// at the highest priority in the backlog, and the reported value is
			// the clamped one the ordering used.
			first := resp.Msg.Entries[0]
			Expect(first.Session.SessionId).To(Equal("waiting"))
			Expect(first.Session.Priority).To(Equal(int32(0)))
			Expect(first.EffectivePriority).To(Equal(int32(2)))
		})

		It("keeps positions relative to the whole backlog when filtered", func() {
			queued("a1", "alpha", 5, base)
			queued("b1", "beta", 3, base)
			queued("a2", "alpha", 1, base)

			svc := build(orchestrator.DispatchPolicy{})
			resp, err := svc.ListQueue(ctx, connect.NewRequest(&v1.ListQueueRequest{Project: "beta"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Entries).To(HaveLen(1))
			Expect(resp.Msg.Entries[0].Session.SessionId).To(Equal("b1"))
			// Second overall, even though it is the only row returned.
			Expect(resp.Msg.Entries[0].Position).To(Equal(int32(2)))
			Expect(resp.Msg.Backlog).To(Equal(int32(3)))
		})

		It("hides entries belonging to projects the key does not cover", func() {
			queued("mine", "alpha", 5, base)
			queued("theirs", "beta", 9, base)

			keys := &memAPIKeyStore{keys: map[string]*apikey.APIKey{}}
			token := addKey(keys, "scoped", "alpha")
			svc := orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{
				SessionMgr:  mgr,
				APIKeyStore: keys,
				AuthEnabled: true,
			})
			DeferCleanup(func() { svc.Shutdown(context.Background()) })
			addr := serveOrchestrator(svc, auth.NewInterceptor(keys))

			resp, err := client.NewOrchestrator(addr, token).ListQueue(ctx,
				connect.NewRequest(&v1.ListQueueRequest{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Entries).To(HaveLen(1))
			Expect(resp.Msg.Entries[0].Session.SessionId).To(Equal("mine"))
			// The higher-priority entry it cannot see is still ahead of it, and
			// the position says so rather than pretending it is next.
			Expect(resp.Msg.Entries[0].Position).To(Equal(int32(2)))
		})
	})

	Describe("GetQueueStats", func() {
		It("reports backlog depth against its bound with a per-project split", func() {
			queued("a1", "alpha", 0, base)
			queued("a2", "alpha", 0, base)
			queued("b1", "beta", 0, base)

			svc := build(orchestrator.DispatchPolicy{MaxBacklog: 500, MaxDispatched: 8})
			resp, err := svc.GetQueueStats(ctx, connect.NewRequest(&v1.GetQueueStatsRequest{}))
			Expect(err).NotTo(HaveOccurred())

			Expect(resp.Msg.Backlog).To(Equal(int32(3)))
			Expect(resp.Msg.MaxBacklog).To(Equal(int32(500)))
			Expect(resp.Msg.Dispatched).To(Equal(int32(0)))
			Expect(resp.Msg.MaxDispatched).To(Equal(int32(8)))

			perProject := map[string]int32{}
			for _, p := range resp.Msg.PerProject {
				perProject[p.Project] = p.Backlog
			}
			Expect(perProject).To(Equal(map[string]int32{"alpha": 2, "beta": 1}))
		})

		It("restricts the per-project split to what the key covers", func() {
			queued("a1", "alpha", 0, base)
			queued("b1", "beta", 0, base)

			keys := &memAPIKeyStore{keys: map[string]*apikey.APIKey{}}
			token := addKey(keys, "scoped", "alpha")
			svc := orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{
				SessionMgr:  mgr,
				APIKeyStore: keys,
				AuthEnabled: true,
			})
			DeferCleanup(func() { svc.Shutdown(context.Background()) })
			addr := serveOrchestrator(svc, auth.NewInterceptor(keys))

			resp, err := client.NewOrchestrator(addr, token).GetQueueStats(ctx,
				connect.NewRequest(&v1.GetQueueStatsRequest{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.PerProject).To(HaveLen(1))
			Expect(resp.Msg.PerProject[0].Project).To(Equal("alpha"))
			// The host-wide depth is not per-project data and stays whole.
			Expect(resp.Msg.Backlog).To(Equal(int32(2)))
		})
	})

	Describe("SetJobPriority", func() {
		It("moves a waiting entry to the head of the backlog", func() {
			queued("waiting", "alpha", 1, base)
			queued("ahead", "alpha", 5, base)

			svc := build(orchestrator.DispatchPolicy{})
			resp, err := svc.SetJobPriority(ctx, connect.NewRequest(&v1.SetJobPriorityRequest{
				SessionId: "waiting",
				Priority:  9,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.PreviousPriority).To(Equal(int32(1)))

			listed, err := svc.ListQueue(ctx, connect.NewRequest(&v1.ListQueueRequest{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(listed.Msg.Entries[0].Session.SessionId).To(Equal("waiting"))
		})

		It("refuses a job that has already been dispatched", func() {
			queued("started", "alpha", 1, base)
			Expect(mgr.UpdateState(ctx, "started", session.StateRunning, "")).To(Succeed())

			svc := build(orchestrator.DispatchPolicy{})
			_, err := svc.SetJobPriority(ctx, connect.NewRequest(&v1.SetJobPriorityRequest{
				SessionId: "started",
				Priority:  9,
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))

			got, err := mgr.Get(ctx, "started")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Priority).To(Equal(1))
		})
	})

	Describe("ListSessions filters", func() {
		BeforeEach(func() {
			Expect(store.CreateSession(ctx, &session.Session{
				ID: "s-pending", ProjectName: "alpha", Mode: "review", Prompt: "p",
				State: session.StatePending, CreatedAt: base, UpdatedAt: base, QueuedAt: base,
			})).To(Succeed())
			Expect(store.CreateSession(ctx, &session.Session{
				ID: "s-failed", ProjectName: "alpha", Mode: "feedback", Prompt: "p", PRRef: "42",
				State: session.StateFailed, CreatedAt: base.Add(time.Minute), UpdatedAt: base.Add(time.Minute),
			})).To(Succeed())
		})

		It("filters by state, mode, pull request and creation time", func() {
			svc := build(orchestrator.DispatchPolicy{})

			byState, err := svc.ListSessions(ctx, connect.NewRequest(&v1.ListSessionsRequest{
				States: []string{"failed"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(projectIDs(byState.Msg.Sessions)).To(Equal([]string{"s-failed"}))

			byMode, err := svc.ListSessions(ctx, connect.NewRequest(&v1.ListSessionsRequest{
				Mode: "review",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(projectIDs(byMode.Msg.Sessions)).To(Equal([]string{"s-pending"}))

			byPR, err := svc.ListSessions(ctx, connect.NewRequest(&v1.ListSessionsRequest{
				PrRef: "42",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(projectIDs(byPR.Msg.Sessions)).To(Equal([]string{"s-failed"}))

			since, err := svc.ListSessions(ctx, connect.NewRequest(&v1.ListSessionsRequest{
				CreatedAfter: timestamppb.New(base.Add(30 * time.Second)),
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(projectIDs(since.Msg.Sessions)).To(Equal([]string{"s-failed"}))
		})

		It("rejects a state no session can be in", func() {
			svc := build(orchestrator.DispatchPolicy{})
			_, err := svc.ListSessions(ctx, connect.NewRequest(&v1.ListSessionsRequest{
				States: []string{"canceled"},
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err).To(MatchError(ContainSubstring("unknown state")))
		})

		It("carries the queue fields a listing is read for", func() {
			svc := build(orchestrator.DispatchPolicy{})
			resp, err := svc.GetSession(ctx, connect.NewRequest(&v1.GetSessionRequest{
				SessionId: "s-pending",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.CreatedAt.AsTime()).To(BeTemporally("~", base, time.Millisecond))
			Expect(resp.Msg.QueuedAt.AsTime()).To(BeTemporally("~", base, time.Millisecond))
			Expect(resp.Msg.Attempts).To(Equal(int32(0)))
		})
	})
})
