package orchestrator_test

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/config/credential"
	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/forge"
	"github.com/aholstenson/kvarn/internal/orchestrator"
	"github.com/aholstenson/kvarn/internal/sandbox"
	"github.com/aholstenson/kvarn/internal/session"
	"github.com/aholstenson/kvarn/internal/vm"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Draining", func() {
	var (
		store      session.Store
		sessionMgr session.Manager
		ag         *gatedAgent
		projStore  *memProjectStore
		tmpDir     string

		// sandboxEntered fires once a run reaches VM creation; sandboxRelease
		// lets it past. Holding a job there is what these specs need and the
		// gated agent cannot give them: a run inside the agent is already
		// `running`, while one waiting on its VM is still in a state a drain
		// may take back.
		sandboxEntered chan string
		sandboxRelease chan struct{}
	)

	build := func(policy orchestrator.DispatchPolicy) *orchestrator.Service {
		factory := func(ctx context.Context, _ sandbox.Opts) (orchestrator.Sandbox, error) {
			select {
			case sandboxEntered <- "":
			default:
			}
			select {
			case <-sandboxRelease:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			wsDir, err := os.MkdirTemp("", "drain-ws-*")
			if err != nil {
				return nil, err
			}
			return &testSandbox{workingDir: wsDir}, nil
		}
		svc := orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{
			CreateOpts:   vm.CreateOpts{},
			ProjectStore: projStore,
			CredentialStore: &memCredentialStore{creds: map[string]*credential.Credential{
				"test-cred": {Name: "test-cred", Config: map[string]string{"token": "ghp_fake"}},
			}},
			ForgeConfigStore: &memForgeConfigStore{configs: map[string]*forgeconfig.ForgeConfig{
				"test-forge": {Name: "test-forge", Type: "mock", Credential: "test-cred"},
			}},
			ForgeTypes:     map[string]forge.Forge{"mock": &mockForge{scmImpl: &mockSCM{}}},
			SessionMgr:     sessionMgr,
			Agent:          ag,
			SandboxFactory: factory,
			Dispatch:       policy,
		})
		DeferCleanup(func() { svc.Shutdown(context.Background()) })
		return svc
	}

	queued := func(id, projectName string) {
		Expect(store.CreateSession(context.Background(), &session.Session{
			ID:          id,
			ProjectName: projectName,
			Prompt:      "do " + id,
			Mode:        "review",
			State:       session.StatePending,
			BaseBranch:  "master",
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
			QueuedAt:    time.Now(),
		})).To(Succeed())
	}

	stateOf := func(id string) func() session.State {
		return func() session.State {
			s, err := sessionMgr.Get(context.Background(), id)
			Expect(err).NotTo(HaveOccurred())
			return s.State
		}
	}

	drain := func(svc *orchestrator.Service, reason string) *v1.SetDrainResponse {
		resp, err := svc.SetDrain(context.Background(), connect.NewRequest(&v1.SetDrainRequest{
			Draining: true, Reason: reason,
		}))
		Expect(err).NotTo(HaveOccurred())
		return resp.Msg
	}

	resume := func(svc *orchestrator.Service) *v1.SetDrainResponse {
		resp, err := svc.SetDrain(context.Background(), connect.NewRequest(&v1.SetDrainRequest{
			Draining: false,
		}))
		Expect(err).NotTo(HaveOccurred())
		return resp.Msg
	}

	stats := func(svc *orchestrator.Service) *v1.GetQueueStatsResponse {
		resp, err := svc.GetQueueStats(context.Background(),
			connect.NewRequest(&v1.GetQueueStatsRequest{}))
		Expect(err).NotTo(HaveOccurred())
		return resp.Msg
	}

	BeforeEach(func() {
		var err error
		tmpDir, err = os.MkdirTemp("", "drain-*")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { os.RemoveAll(tmpDir) })

		store = session.NewMemStore()
		sessionMgr = session.NewManager(store)
		ag = newGatedAgent()
		sandboxEntered = make(chan string, 8)
		sandboxRelease = make(chan struct{})

		projStore = &memProjectStore{projects: map[string]*project.Project{
			"alpha": {
				Name:          "alpha",
				RepoURL:       filepath.Join(tmpDir, "alpha.git"),
				DefaultBranch: "master",
				Forge:         "test-forge",
			},
		}}
	})

	It("leaves the backlog alone while draining and dispatches it on resume", func() {
		svc := build(orchestrator.DispatchPolicy{Interval: 20 * time.Millisecond})
		drain(svc, "deploying")
		close(sandboxRelease)
		close(ag.release)

		queued("waiting", "alpha")

		// The entry is accepted and durable; what a drain stops is it leaving
		// the backlog.
		Consistently(stateOf("waiting"), 500*time.Millisecond).Should(Equal(session.StatePending))

		resume(svc)
		Eventually(stateOf("waiting"), dispatchTimeout).Should(Equal(session.StateCompleted))
	})

	// The point of the requeue: a job that has only read so far costs nothing
	// to re-run, so the host reaches empty in the time its real work takes.
	It("returns a job that has not started running to the backlog", func() {
		queued("booting", "alpha")
		svc := build(orchestrator.DispatchPolicy{Interval: 20 * time.Millisecond})

		Eventually(sandboxEntered, dispatchTimeout).Should(Receive())

		resp := drain(svc, "deploying")
		Expect(resp.Requeued).To(ConsistOf("booting"))
		Eventually(stateOf("booting"), dispatchTimeout).Should(Equal(session.StatePending))

		got, err := sessionMgr.Get(context.Background(), "booting")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Attempts).To(Equal(1))
		Expect(got.Error).To(BeEmpty())

		// Still draining, so it waits rather than starting again immediately.
		Consistently(stateOf("booting"), 300*time.Millisecond).Should(Equal(session.StatePending))

		close(sandboxRelease)
		close(ag.release)
		resume(svc)
		Eventually(stateOf("booting"), dispatchTimeout).Should(Equal(session.StateCompleted))
	})

	// A run inside the agent has spent against its cost cap and holds work that
	// only exists in its VM. Re-running it would spend twice, so a drain lets
	// it finish instead.
	It("lets a job that is already running finish", func() {
		queued("started", "alpha")
		close(sandboxRelease)
		svc := build(orchestrator.DispatchPolicy{Interval: 20 * time.Millisecond})

		Eventually(ag.total, dispatchTimeout).Should(Equal(1))

		resp := drain(svc, "deploying")
		Expect(resp.Requeued).To(BeEmpty())
		Expect(resp.Dispatched).To(Equal(int32(1)))

		Consistently(stateOf("started"), 300*time.Millisecond).Should(Equal(session.StateRunning))

		close(ag.release)
		Eventually(stateOf("started"), dispatchTimeout).Should(Equal(session.StateCompleted))
	})

	It("reports the stance and its reason through queue stats", func() {
		svc := build(orchestrator.DispatchPolicy{Interval: 20 * time.Millisecond})

		before := stats(svc)
		Expect(before.Draining).To(BeFalse())
		Expect(before.DrainingSince).To(BeNil())

		drain(svc, "rolling restart")
		during := stats(svc)
		Expect(during.Draining).To(BeTrue())
		Expect(during.DrainReason).To(Equal("rolling restart"))
		Expect(during.DrainingSince).NotTo(BeNil())

		resume(svc)
		Expect(stats(svc).Draining).To(BeFalse())
	})

	It("tells a caller whether it was the one that drained the host", func() {
		svc := build(orchestrator.DispatchPolicy{Interval: 20 * time.Millisecond})

		Expect(drain(svc, "first").PreviouslyDraining).To(BeFalse())
		Expect(drain(svc, "second").PreviouslyDraining).To(BeTrue())
		Expect(resume(svc).PreviouslyDraining).To(BeTrue())
		Expect(resume(svc).PreviouslyDraining).To(BeFalse())
	})

	// "Since" is when the host stopped taking work; a second drain call is not
	// a new event and must not restart the clock an operator is reading.
	It("keeps the original timestamp when drained twice", func() {
		svc := build(orchestrator.DispatchPolicy{Interval: 20 * time.Millisecond})

		drain(svc, "first")
		first := stats(svc).DrainingSince.AsTime()

		time.Sleep(10 * time.Millisecond)
		drain(svc, "second")
		Expect(stats(svc).DrainingSince.AsTime()).To(BeTemporally("==", first))
		Expect(stats(svc).DrainReason).To(Equal("second"))
	})

	// A drain must not push a job past the cap that stops a job which kills the
	// host from being retried forever.
	It("cancels rather than requeues a job that has used up its attempts", func() {
		Expect(store.CreateSession(context.Background(), &session.Session{
			ID:          "spent",
			ProjectName: "alpha",
			Prompt:      "do spent",
			Mode:        "review",
			State:       session.StatePending,
			BaseBranch:  "master",
			// Already on its last permitted dispatch, so there is nothing left
			// to requeue it into.
			Attempts:  3,
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
			QueuedAt:  time.Now(),
		})).To(Succeed())

		svc := build(orchestrator.DispatchPolicy{Interval: 20 * time.Millisecond, MaxAttempts: 3})
		Eventually(sandboxEntered, dispatchTimeout).Should(Receive())

		drain(svc, "deploying")
		Eventually(stateOf("spent"), dispatchTimeout).Should(Equal(session.StateCancelled))
	})

	// The backlog is what a drain exists to protect, so the expiry sweep stands
	// down with the rest of the dispatcher rather than failing entries for a
	// wait the operator imposed.
	It("does not expire backlog entries while draining", func() {
		svc := build(orchestrator.DispatchPolicy{
			Interval:     20 * time.Millisecond,
			MaxQueueWait: time.Hour,
		})
		drain(svc, "deploying")
		// A pass that began before the drain was set runs to its end, so give
		// the one in flight time to finish before introducing the entry this
		// spec is about. What is being asserted is that no *later* pass touches
		// it, which is the guarantee the drain actually makes.
		time.Sleep(100 * time.Millisecond)

		Expect(store.CreateSession(context.Background(), &session.Session{
			ID:          "old",
			ProjectName: "alpha",
			Prompt:      "do old",
			Mode:        "review",
			State:       session.StatePending,
			BaseBranch:  "master",
			CreatedAt:   time.Now().Add(-2 * time.Hour),
			UpdatedAt:   time.Now().Add(-2 * time.Hour),
			QueuedAt:    time.Now().Add(-2 * time.Hour),
		})).To(Succeed())

		Consistently(stateOf("old"), 500*time.Millisecond).Should(Equal(session.StatePending))

		// It expires as normal once dispatch resumes.
		resume(svc)
		Eventually(stateOf("old"), dispatchTimeout).Should(Equal(session.StateFailed))
	})
})
