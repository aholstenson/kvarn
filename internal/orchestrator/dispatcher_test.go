package orchestrator_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/config/credential"
	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/forge"
	"github.com/aholstenson/kvarn/internal/orchestrator"
	"github.com/aholstenson/kvarn/internal/sandbox"
	"github.com/aholstenson/kvarn/internal/session"
	"github.com/aholstenson/kvarn/internal/vm"
)

// gatedAgent holds every run it starts until the test releases it, so a test
// can observe how many jobs the dispatcher has in flight at once. Unlike
// blockingAgent it supports any number of concurrent runs.
type gatedAgent struct {
	release chan struct{}

	mu      sync.Mutex
	running map[string]int
}

func newGatedAgent() *gatedAgent {
	return &gatedAgent{release: make(chan struct{}), running: map[string]int{}}
}

func (a *gatedAgent) Start(_ context.Context, actx *agent.Context) (agent.Conversation, error) {
	return &gatedConversation{a: a, project: actx.ProjectName}, nil
}

// byProject reports how many runs are currently inside the agent, per project.
func (a *gatedAgent) byProject() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]int, len(a.running))
	for k, v := range a.running {
		out[k] = v
	}
	return out
}

func (a *gatedAgent) total() int {
	n := 0
	for _, v := range a.byProject() {
		n += v
	}
	return n
}

type gatedConversation struct {
	a       *gatedAgent
	project string
}

func (c *gatedConversation) Run(ctx context.Context, _ string) (string, error) {
	c.a.mu.Lock()
	c.a.running[c.project]++
	c.a.mu.Unlock()
	defer func() {
		c.a.mu.Lock()
		c.a.running[c.project]--
		c.a.mu.Unlock()
	}()

	select {
	case <-c.a.release:
		return "done", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (c *gatedConversation) Summarize(_ context.Context) (*agent.Result, error) {
	return &agent.Result{Title: "gated"}, nil
}

func (c *gatedConversation) Close() error { return nil }

// dispatchTimeout is the window these specs allow for a job to travel from the
// backlog to wherever they assert about it. It is generous on purpose: the whole
// suite runs alongside 40-odd others, so a job that normally lands in
// milliseconds can be starved for far longer than Gomega's one-second default.
const dispatchTimeout = 10 * time.Second

var _ = Describe("Backlog dispatch", func() {
	var (
		store      session.Store
		sessionMgr session.Manager
		ag         *gatedAgent
		projStore  *memProjectStore
		tmpDir     string
	)

	// build assembles a service over the fixtures above with the given dispatch
	// bounds. Each test builds its own so the policy can differ.
	build := func(policy orchestrator.DispatchPolicy) *orchestrator.Service {
		factory := func(_ context.Context, _ sandbox.Opts) (orchestrator.Sandbox, error) {
			wsDir, err := os.MkdirTemp("", "dispatch-ws-*")
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

	// queued writes a backlog entry directly, so a test can control the queue
	// age and state a real submission would not let it set. The mode is
	// read-only: these tests are about which jobs start and when, so a run that
	// stops short of opening a pull request is all they need.
	queued := func(id, projectName string, state session.State, queuedAt time.Time) *session.Session {
		s := &session.Session{
			ID:          id,
			ProjectName: projectName,
			Prompt:      "do " + id,
			Mode:        "review",
			State:       state,
			BaseBranch:  "master",
			CreatedAt:   queuedAt,
			UpdatedAt:   queuedAt,
			QueuedAt:    queuedAt,
		}
		Expect(store.CreateSession(context.Background(), s)).To(Succeed())
		return s
	}

	stateOf := func(id string) func() session.State {
		return func() session.State {
			s, err := sessionMgr.Get(context.Background(), id)
			Expect(err).NotTo(HaveOccurred())
			return s.State
		}
	}

	BeforeEach(func() {
		var err error
		tmpDir, err = os.MkdirTemp("", "dispatcher-*")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { os.RemoveAll(tmpDir) })

		store = session.NewMemStore()
		sessionMgr = session.NewManager(store)
		ag = newGatedAgent()

		projStore = &memProjectStore{projects: map[string]*project.Project{}}
		for _, name := range []string{"alpha", "beta"} {
			projStore.projects[name] = &project.Project{
				Name:          name,
				RepoURL:       filepath.Join(tmpDir, name+".git"),
				DefaultBranch: "master",
				Forge:         "test-forge",
			}
		}
	})

	It("runs a job the restart handed back to the backlog", func() {
		// The shape startup reconciliation leaves: a run interrupted before it
		// spent anything, returned to the backlog with an attempt charged.
		queued("interrupted", "alpha", session.StateSetup, time.Now())
		res, err := store.ReconcileStartup(context.Background(), session.ReconcileOpts{
			RequeueMessage: "requeued", FailError: "orchestrator restarted",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Requeued).To(ConsistOf("interrupted"))

		build(orchestrator.DispatchPolicy{Interval: 20 * time.Millisecond})
		close(ag.release)

		Eventually(stateOf("interrupted"), dispatchTimeout).Should(Equal(session.StateCompleted))
		got, err := sessionMgr.Get(context.Background(), "interrupted")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Attempts).To(Equal(1))
	})

	It("fails a queued job whose project was deleted while it waited", func() {
		queued("orphaned", "gone", session.StatePending, time.Now())

		build(orchestrator.DispatchPolicy{Interval: 20 * time.Millisecond})

		Eventually(stateOf("orphaned"), dispatchTimeout).Should(Equal(session.StateFailed))
		got, err := sessionMgr.Get(context.Background(), "orphaned")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Error).To(ContainSubstring("gone"))
	})

	It("expires a backlog entry nobody is waiting for any more", func() {
		queued("stale", "alpha", session.StatePending, time.Now().Add(-2*time.Hour))
		queued("fresh", "alpha", session.StatePending, time.Now())
		close(ag.release)

		build(orchestrator.DispatchPolicy{
			Interval:     20 * time.Millisecond,
			MaxQueueWait: time.Hour,
		})

		Eventually(stateOf("stale"), dispatchTimeout).Should(Equal(session.StateFailed))
		got, err := sessionMgr.Get(context.Background(), "stale")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Error).To(ContainSubstring("queued longer than"))

		// The entry still inside the window runs as normal.
		Eventually(stateOf("fresh"), dispatchTimeout).Should(Equal(session.StateCompleted))
	})

	It("holds the pipeline at its bound and admits the rest as slots free", func() {
		for i := range 3 {
			queued(fmt.Sprintf("job%d", i), "alpha", session.StatePending,
				time.Now().Add(time.Duration(i)*time.Second))
		}

		build(orchestrator.DispatchPolicy{MaxDispatched: 1, Interval: 20 * time.Millisecond})

		// One job reaches the agent and the other two stay in the backlog.
		Eventually(ag.total, dispatchTimeout).Should(Equal(1))
		Consistently(ag.total, 200*time.Millisecond).Should(Equal(1))

		close(ag.release)
		for i := range 3 {
			Eventually(stateOf(fmt.Sprintf("job%d", i)), dispatchTimeout).Should(Equal(session.StateCompleted))
		}
	})

	It("gives a newly arrived project a share rather than draining the first", func() {
		// alpha queued four jobs before beta queued one, so pure arrival order
		// would fill both slots with alpha and leave beta waiting behind work
		// that has not started yet.
		base := time.Now().Add(-time.Minute)
		for i := range 4 {
			queued(fmt.Sprintf("alpha%d", i), "alpha", session.StatePending,
				base.Add(time.Duration(i)*time.Second))
		}
		queued("beta0", "beta", session.StatePending, time.Now())

		build(orchestrator.DispatchPolicy{MaxDispatched: 2, Interval: 20 * time.Millisecond})

		// Two projects want the pipeline, so each is capped at one of its two
		// slots and beta starts without waiting for alpha's queue to drain.
		Eventually(ag.byProject, dispatchTimeout).Should(Equal(map[string]int{"alpha": 1, "beta": 1}))

		close(ag.release)
		Eventually(stateOf("beta0"), dispatchTimeout).Should(Equal(session.StateCompleted))
	})
})
