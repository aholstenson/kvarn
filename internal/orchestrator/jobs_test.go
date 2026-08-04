package orchestrator_test

import (
	"context"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/orchestrator"
	"github.com/aholstenson/kvarn/internal/session"
)

var _ = Describe("Bulk cancel", func() {
	var (
		ctx   context.Context
		store session.Store
		mgr   session.Manager
		svc   *orchestrator.Service
	)

	// A backlog with no project store behind it stays pending, which is what
	// lets these specs assert on what a sweep selected rather than racing the
	// dispatcher for it.
	BeforeEach(func() {
		ctx = context.Background()
		store = session.NewMemStore()
		mgr = session.NewManager(store)

		now := time.Now()
		for _, s := range []*session.Session{
			{ID: "a1", ProjectName: "alpha", Mode: "review", Prompt: "p", State: session.StatePending},
			{ID: "a2", ProjectName: "alpha", Mode: "implement", Prompt: "p", State: session.StatePending},
			{ID: "b1", ProjectName: "beta", Mode: "review", Prompt: "p", State: session.StatePending},
			{ID: "done", ProjectName: "alpha", Mode: "review", Prompt: "p", State: session.StateCompleted},
		} {
			s.CreatedAt, s.UpdatedAt, s.QueuedAt = now, now, now
			Expect(store.CreateSession(ctx, s)).To(Succeed())
		}

		svc = orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{SessionMgr: mgr})
		DeferCleanup(func() { svc.Shutdown(context.Background()) })
	})

	stateOf := func(id string) session.State {
		s, err := mgr.Get(ctx, id)
		Expect(err).NotTo(HaveOccurred())
		return s.State
	}

	It("cancels every active job for one project and leaves the rest", func() {
		resp, err := svc.CancelJobs(ctx, connect.NewRequest(&v1.CancelJobsRequest{
			Project: "alpha",
			Reason:  "draining",
		}))
		Expect(err).NotTo(HaveOccurred())

		ids := []string{}
		for _, j := range resp.Msg.Jobs {
			Expect(j.Error).To(BeEmpty())
			Expect(j.PreviousState).To(Equal(string(session.StatePending)))
			ids = append(ids, j.SessionId)
		}
		Expect(ids).To(ConsistOf("a1", "a2"))

		Expect(stateOf("a1")).To(Equal(session.StateCancelled))
		Expect(stateOf("b1")).To(Equal(session.StatePending))
		Expect(stateOf("done")).To(Equal(session.StateCompleted))

		cancelled, err := mgr.Get(ctx, "a1")
		Expect(err).NotTo(HaveOccurred())
		Expect(cancelled.Message).To(ContainSubstring("draining"))
	})

	It("narrows by mode as well as project", func() {
		resp, err := svc.CancelJobs(ctx, connect.NewRequest(&v1.CancelJobsRequest{
			Project: "alpha",
			Mode:    "implement",
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.Jobs).To(HaveLen(1))
		Expect(resp.Msg.Jobs[0].SessionId).To(Equal("a2"))
		Expect(stateOf("a1")).To(Equal(session.StatePending))
	})

	It("reports what a dry run would cancel without cancelling it", func() {
		resp, err := svc.CancelJobs(ctx, connect.NewRequest(&v1.CancelJobsRequest{
			Project: "alpha",
			DryRun:  true,
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.Jobs).To(HaveLen(2))
		Expect(stateOf("a1")).To(Equal(session.StatePending))
		Expect(stateOf("a2")).To(Equal(session.StatePending))
	})

	It("refuses an unfiltered sweep unless it says so", func() {
		_, err := svc.CancelJobs(ctx, connect.NewRequest(&v1.CancelJobsRequest{}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(stateOf("a1")).To(Equal(session.StatePending))

		resp, err := svc.CancelJobs(ctx, connect.NewRequest(&v1.CancelJobsRequest{All: true}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.Jobs).To(HaveLen(3))
	})

	It("rejects a terminal state, which has nothing to cancel", func() {
		_, err := svc.CancelJobs(ctx, connect.NewRequest(&v1.CancelJobsRequest{
			States: []string{"completed"},
		}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
	})
})

var _ = Describe("RetryJob", func() {
	var (
		ctx context.Context
		mgr session.Manager
		svc *orchestrator.Service
	)

	BeforeEach(func() {
		ctx = context.Background()
		mgr = session.NewManager(session.NewMemStore())

		projStore := &memProjectStore{projects: map[string]*project.Project{
			"alpha": {
				Name:          "alpha",
				RepoURL:       filepath.Join(GinkgoT().TempDir(), "alpha.git"),
				DefaultBranch: "master",
			},
		}}
		svc = orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{
			SessionMgr:   mgr,
			ProjectStore: projStore,
		})
		DeferCleanup(func() { svc.Shutdown(context.Background()) })
	})

	// finished creates a session and drives it to a terminal state, which is
	// the only state a retry accepts.
	finished := func(mode, prRef string) *session.Session {
		sess, err := mgr.Create(ctx, session.CreateParams{
			ProjectName: "alpha",
			Prompt:      "fix the bug",
			Mode:        mode,
			BaseBranch:  "release",
			PRRef:       prRef,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(mgr.Fail(ctx, sess.ID, context.Canceled)).To(Succeed())
		return sess
	}

	It("resubmits a failed job as a new session, leaving the original alone", func() {
		original := finished("implement", "")

		resp, err := svc.RetryJob(ctx, connect.NewRequest(&v1.RetryJobRequest{
			SessionId: original.ID,
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.SessionId).NotTo(Equal(original.ID))

		retried, err := mgr.Get(ctx, resp.Msg.SessionId)
		Expect(err).NotTo(HaveOccurred())
		Expect(retried.ProjectName).To(Equal("alpha"))
		Expect(retried.Prompt).To(Equal("fix the bug"))
		Expect(retried.Mode).To(Equal("implement"))
		// The branch the original was submitted against, not the project
		// default it would fall back to.
		Expect(retried.BaseBranch).To(Equal("release"))

		// The record of what happened is not rewritten by trying again.
		unchanged, err := mgr.Get(ctx, original.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(unchanged.State).To(Equal(session.StateFailed))
	})

	It("replaces the prompt when the caller supplies one", func() {
		original := finished("implement", "")

		resp, err := svc.RetryJob(ctx, connect.NewRequest(&v1.RetryJobRequest{
			SessionId: original.ID,
			Prompt:    "fix it properly this time",
		}))
		Expect(err).NotTo(HaveOccurred())

		retried, err := mgr.Get(ctx, resp.Msg.SessionId)
		Expect(err).NotTo(HaveOccurred())
		Expect(retried.Prompt).To(Equal("fix it properly this time"))
	})

	It("refuses a job that has not finished", func() {
		sess, err := mgr.Create(ctx, session.CreateParams{
			ProjectName: "alpha", Prompt: "p", Mode: "implement",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(mgr.UpdateState(ctx, sess.ID, session.StateRunning, "")).To(Succeed())

		_, err = svc.RetryJob(ctx, connect.NewRequest(&v1.RetryJobRequest{SessionId: sess.ID}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err).To(MatchError(ContainSubstring("cancel it before retrying")))
	})

	It("refuses a job that already opened a pull request", func() {
		// Resubmitting this would open a second pull request for one task.
		original := finished("implement", "42")

		_, err := svc.RetryJob(ctx, connect.NewRequest(&v1.RetryJobRequest{SessionId: original.ID}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err).To(MatchError(ContainSubstring("feedback")))
	})
})
