package orchestrator_test

import (
	"context"
	"os"
	"path/filepath"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
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

// ctxCheckStore refuses every operation on a cancelled context, the way a real
// database driver does. The in-memory store ignores its context entirely, which
// would hide a terminal-state write issued on the job's own (dead) context.
type ctxCheckStore struct {
	session.Store
}

func (s *ctxCheckStore) GetSession(ctx context.Context, id string) (*session.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.Store.GetSession(ctx, id)
}

func (s *ctxCheckStore) UpdateSession(ctx context.Context, sess *session.Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.UpdateSession(ctx, sess)
}

func (s *ctxCheckStore) AppendEvent(ctx context.Context, sessionID, kind string, payload []byte) (session.PersistedEvent, error) {
	if err := ctx.Err(); err != nil {
		return session.PersistedEvent{}, err
	}
	return s.Store.AppendEvent(ctx, sessionID, kind, payload)
}

// blockingAgent runs until the job context is cancelled and then reports the
// cancellation as a failure, the shape of an agent interrupted by shutdown or
// a tripped cost cap.
type blockingAgent struct {
	started chan struct{}
}

func (a *blockingAgent) Start(_ context.Context, _ *agent.Context) (agent.Conversation, error) {
	return &blockingConversation{a: a}, nil
}

type blockingConversation struct {
	a *blockingAgent
}

func (c *blockingConversation) Run(ctx context.Context, _ string) (string, error) {
	close(c.a.started)
	<-ctx.Done()
	return "", ctx.Err()
}

func (c *blockingConversation) Summarize(_ context.Context) (*agent.Result, error) {
	return &agent.Result{Title: "unused"}, nil
}

func (c *blockingConversation) Close() error { return nil }

var _ = Describe("Terminal state persistence", func() {
	var (
		svc        *orchestrator.Service
		sessionMgr session.Manager
		ag         *blockingAgent
		tmpDir     string
	)

	BeforeEach(func() {
		var err error
		tmpDir, err = os.MkdirTemp("", "terminal-state-*")
		Expect(err).NotTo(HaveOccurred())

		sessionMgr = session.NewManager(&ctxCheckStore{Store: session.NewMemStore()})
		ag = &blockingAgent{started: make(chan struct{})}

		projStore := &memProjectStore{
			projects: map[string]*project.Project{
				"test-project": {
					Name:          "test-project",
					RepoURL:       filepath.Join(tmpDir, "repo.git"),
					DefaultBranch: "master",
					Forge:         "test-forge",
				},
			},
		}
		forgeConfigStore := &memForgeConfigStore{
			configs: map[string]*forgeconfig.ForgeConfig{
				"test-forge": {Name: "test-forge", Type: "mock", Credential: "test-cred"},
			},
		}
		credStore := &memCredentialStore{
			creds: map[string]*credential.Credential{
				"test-cred": {Name: "test-cred", Config: map[string]string{"token": "ghp_fake"}},
			},
		}

		factory := func(_ context.Context, _ sandbox.Opts) (orchestrator.Sandbox, error) {
			wsDir, err := os.MkdirTemp("", "terminal-state-ws-*")
			if err != nil {
				return nil, err
			}
			return &testSandbox{workingDir: wsDir}, nil
		}

		svc = orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{
			CreateOpts:       vm.CreateOpts{},
			ProjectStore:     projStore,
			CredentialStore:  credStore,
			ForgeConfigStore: forgeConfigStore,
			ForgeTypes:       map[string]forge.Forge{"mock": &mockForge{scmImpl: &mockSCM{}}},
			SessionMgr:       sessionMgr,
			Agent:            ag,
			SandboxFactory:   factory,
		})
	})

	AfterEach(func() {
		os.RemoveAll(tmpDir)
	})

	It("records the failure when the job context is already cancelled", func() {
		resp, err := svc.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "do the thing",
		}))
		Expect(err).NotTo(HaveOccurred())
		sessionID := resp.Msg.SessionId

		Eventually(ag.started).Should(BeClosed())

		// Shutdown cancels the job context out from under the agent. The
		// resulting Fail must still land, or the session would stay
		// non-terminal until the next startup reconciliation.
		svc.Shutdown(context.Background())

		sess, err := sessionMgr.Get(context.Background(), sessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(sess.State).To(Equal(session.StateFailed))
		Expect(sess.Error).To(ContainSubstring("agent:"))
	})
})
