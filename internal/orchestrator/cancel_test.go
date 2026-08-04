package orchestrator_test

import (
	"context"
	"os"
	"path/filepath"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/config/credential"
	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/forge"
	"github.com/aholstenson/kvarn/internal/orchestrator"
	"github.com/aholstenson/kvarn/internal/sandbox"
	"github.com/aholstenson/kvarn/internal/session"
	"github.com/aholstenson/kvarn/internal/vm"
)

var _ = Describe("CancelJob", func() {
	var (
		svc        *orchestrator.Service
		sessionMgr session.Manager
		ag         *blockingAgent
		tmpDir     string
	)

	BeforeEach(func() {
		var err error
		tmpDir, err = os.MkdirTemp("", "cancel-job-*")
		Expect(err).NotTo(HaveOccurred())

		sessionMgr = session.NewManager(session.NewMemStore())
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
			wsDir, err := os.MkdirTemp("", "cancel-job-ws-*")
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
		svc.Shutdown(context.Background())
		os.RemoveAll(tmpDir)
	})

	startRunningJob := func() string {
		resp, err := svc.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "do the thing",
		}))
		Expect(err).NotTo(HaveOccurred())
		Eventually(ag.started).Should(BeClosed())
		return resp.Msg.SessionId
	}

	It("stops a running job and records it as cancelled", func() {
		sessionID := startRunningJob()

		resp, err := svc.CancelJob(context.Background(), connect.NewRequest(&v1.CancelJobRequest{
			SessionId: sessionID,
			Reason:    "wrong prompt",
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.PreviousState).To(Equal(string(session.StateRunning)))

		// The run unwinds on its own; the state lands once it has torn down.
		Eventually(func() session.State {
			sess, err := sessionMgr.Get(context.Background(), sessionID)
			Expect(err).NotTo(HaveOccurred())
			return sess.State
		}).Should(Equal(session.StateCancelled))

		sess, err := sessionMgr.Get(context.Background(), sessionID)
		Expect(err).NotTo(HaveOccurred())
		// A cancellation is not a failure: the reason is the session's message,
		// and the error field stays empty.
		Expect(sess.Message).To(ContainSubstring("wrong prompt"))
		Expect(sess.Error).To(BeEmpty())
		Expect(sess.State.IsTerminal()).To(BeTrue())
	})

	It("closes watchers when the job is cancelled", func() {
		sessionID := startRunningJob()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch, err := sessionMgr.Watch(ctx, sessionID, 0)
		Expect(err).NotTo(HaveOccurred())

		_, err = svc.CancelJob(context.Background(), connect.NewRequest(&v1.CancelJobRequest{
			SessionId: sessionID,
		}))
		Expect(err).NotTo(HaveOccurred())

		// A watching client (`kvarn startjob --watch`) has to come back rather
		// than hang, so the cancelled state has to close the stream the way
		// completed and failed do. Polling BeClosed drains the events on its way.
		Eventually(ch).Should(BeClosed())

		sess, err := sessionMgr.Get(context.Background(), sessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(sess.State).To(Equal(session.StateCancelled))
	})

	It("rejects an unknown session", func() {
		_, err := svc.CancelJob(context.Background(), connect.NewRequest(&v1.CancelJobRequest{
			SessionId: "does-not-exist",
		}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
	})

	It("rejects a session that has already finished", func() {
		sess, err := sessionMgr.Create(context.Background(), session.CreateParams{
			ProjectName: "test-project", Prompt: "done", Mode: "auto",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(sessionMgr.UpdateState(context.Background(), sess.ID, session.StateCompleted, "Completed")).To(Succeed())

		_, err = svc.CancelJob(context.Background(), connect.NewRequest(&v1.CancelJobRequest{
			SessionId: sess.ID,
		}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("already finished"))
	})

	It("rejects a mid-run session with no run behind it", func() {
		sess, err := sessionMgr.Create(context.Background(), session.CreateParams{
			ProjectName: "test-project", Prompt: "orphan", Mode: "auto",
		})
		Expect(err).NotTo(HaveOccurred())
		// Past the backlog but with no goroutine behind it — the shape a
		// terminal write that failed outright leaves, since startup
		// reconciliation settles every other way of getting here.
		Expect(sessionMgr.UpdateState(context.Background(), sess.ID, session.StateCloning, "")).To(Succeed())

		_, err = svc.CancelJob(context.Background(), connect.NewRequest(&v1.CancelJobRequest{
			SessionId: sess.ID,
		}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("not running"))
	})

	It("cancels a job still waiting in the backlog", func() {
		sess, err := sessionMgr.Create(context.Background(), session.CreateParams{
			ProjectName: "test-project", Prompt: "queued", Mode: "auto",
		})
		Expect(err).NotTo(HaveOccurred())

		// No goroutine owns a backlog entry, so the cancel lands on the row
		// itself rather than on a context.
		resp, err := svc.CancelJob(context.Background(), connect.NewRequest(&v1.CancelJobRequest{
			SessionId: sess.ID, Reason: "changed my mind",
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.PreviousState).To(Equal(string(session.StatePending)))

		got, err := sessionMgr.Get(context.Background(), sess.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.State).To(Equal(session.StateCancelled))
		Expect(got.Message).To(ContainSubstring("changed my mind"))
		Expect(got.Error).To(BeEmpty())
	})

	It("cancels a job that has not reached the agent yet", func() {
		// The registration happens before the job goroutine starts, so a cancel
		// issued the moment StartJob returns cannot slip past it.
		resp, err := svc.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "do the thing",
		}))
		Expect(err).NotTo(HaveOccurred())
		sessionID := resp.Msg.SessionId

		_, err = svc.CancelJob(context.Background(), connect.NewRequest(&v1.CancelJobRequest{
			SessionId: sessionID,
		}))
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() session.State {
			sess, err := sessionMgr.Get(context.Background(), sessionID)
			Expect(err).NotTo(HaveOccurred())
			return sess.State
		}).Should(Equal(session.StateCancelled))
	})
})
