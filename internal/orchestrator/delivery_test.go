package orchestrator_test

import (
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/config/credential"
	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/forge"
	"github.com/aholstenson/kvarn/internal/orchestrator"
	"github.com/aholstenson/kvarn/internal/runner"
	"github.com/aholstenson/kvarn/internal/sandbox"
	"github.com/aholstenson/kvarn/internal/session"
	"github.com/aholstenson/kvarn/internal/vm"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// unreachable is a push failure the way git reports a forge it cannot reach.
var unreachable = stderrors.New("fatal: unable to access 'https://github.com/test/repo.git/': Could not resolve host: github.com")

var _ = Describe("Delivery retries", func() {
	var (
		tmpDir     string
		jobsDir    string
		mockScm    *mockSCM
		forgeInst  *mockForge
		sessionMgr session.Manager
		testAgent  *scriptedAgent

		wsMu       sync.Mutex
		workspaces []string
	)

	BeforeEach(func() {
		var err error
		tmpDir, err = os.MkdirTemp("", "delivery-test-*")
		Expect(err).NotTo(HaveOccurred())
		jobsDir = filepath.Join(tmpDir, "jobs")
		mockScm = &mockSCM{}
		forgeInst = &mockForge{scmImpl: mockScm}
		sessionMgr = session.NewManager(session.NewMemStore())
		testAgent = &scriptedAgent{
			title:       "Update README greeting",
			description: "Updated the README with a friendly greeting.",
			fileName:    "README.md",
			fileBody:    "# Test\n\nhi there\n",
		}
		workspaces = nil
		DeferCleanup(orchestrator.SetDeliveryBackoffForTest(20 * time.Millisecond))
		DeferCleanup(func() { _ = os.RemoveAll(tmpDir) })
	})

	newService := func(window time.Duration) *orchestrator.Service {
		factory := func(_ context.Context, opts sandbox.Opts) (orchestrator.Sandbox, error) {
			wsDir, err := os.MkdirTemp("", "test-workspace-*")
			if err != nil {
				return nil, err
			}
			if opts.SourceDir != "" {
				if out, err := exec.Command("cp", "-a", opts.SourceDir+"/.", wsDir).CombinedOutput(); err != nil {
					return nil, fmt.Errorf("copy source: %s: %w", out, err)
				}
			}
			if _, err := os.Stat(filepath.Join(wsDir, ".git")); os.IsNotExist(err) {
				for _, args := range [][]string{
					{"init", wsDir},
					{"-C", wsDir, "add", "."},
					{"-C", wsDir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init"},
				} {
					if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
						return nil, fmt.Errorf("git %v: %s: %w", args, out, err)
					}
				}
			}
			wsMu.Lock()
			workspaces = append(workspaces, wsDir)
			wsMu.Unlock()
			proxy := &localRunnerProxy{handler: runner.NewUnprivilegedHandler()}
			sessResp, err := proxy.CreateSession(context.Background(), &v1.CreateSessionRequest{WorkingDir: wsDir})
			if err != nil {
				return nil, err
			}
			return newTestSandbox(proxy, sessResp.SessionId, wsDir), nil
		}

		svc := orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{
			CreateOpts: vm.CreateOpts{},
			ProjectStore: &memProjectStore{projects: map[string]*project.Project{
				"test-project": {
					Name:          "test-project",
					RepoURL:       filepath.Join(tmpDir, "repo.git"),
					DefaultBranch: "master",
					Forge:         "test-forge",
					Labels:        []string{"kvarn"},
				},
			}},
			CredentialStore: &memCredentialStore{creds: map[string]*credential.Credential{
				"test-cred": {Name: "test-cred", Config: map[string]string{"token": "ghp_fake"}},
			}},
			ForgeConfigStore: &memForgeConfigStore{configs: map[string]*forgeconfig.ForgeConfig{
				"test-forge": {Name: "test-forge", Type: "mock", Credential: "test-cred"},
			}},
			ForgeTypes:          map[string]forge.Forge{"mock": forgeInst},
			SessionMgr:          sessionMgr,
			Agent:               testAgent,
			SandboxFactory:      factory,
			JobsDir:             jobsDir,
			DeliveryRetryWindow: window,
		})
		DeferCleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			svc.Shutdown(ctx)
		})
		return svc
	}

	start := func(svc *orchestrator.Service) string {
		resp, err := svc.StartJob(context.Background(), &v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "Please add a greeting file.",
		})
		Expect(err).NotTo(HaveOccurred())
		return resp.SessionId
	}

	sessionOf := func(id string) func() *session.Session {
		return func() *session.Session {
			s, err := sessionMgr.Get(context.Background(), id)
			if err != nil {
				return nil
			}
			return s
		}
	}
	stateOf := func(id string) func() session.State {
		return func() session.State {
			if s := sessionOf(id)(); s != nil {
				return s.State
			}
			return ""
		}
	}
	pendingClones := func() []os.DirEntry {
		entries, _ := os.ReadDir(filepath.Join(jobsDir, "pending"))
		return entries
	}

	It("retries a push that cannot reach the forge and then opens the pull request", func() {
		mockScm.pushErrs = []error{unreachable, unreachable}
		var vmClosedFirst bool
		mockScm.onPush = func(pushRecord) {
			if mockScm.pushCalls == 1 {
				wsMu.Lock()
				ws := workspaces[len(workspaces)-1]
				wsMu.Unlock()
				_, err := os.Stat(ws)
				vmClosedFirst = os.IsNotExist(err)
			}
		}

		id := start(newService(0))
		Eventually(stateOf(id), 5*time.Second).Should(Equal(session.StateCompleted))

		sess := sessionOf(id)()
		Expect(sess.PullRequestURL).To(Equal("https://github.com/test/repo/pull/1"))
		Expect(mockScm.pushCalls).To(Equal(3))
		Expect(forgeInst.prCalls).To(Equal(1))
		Expect(forgeInst.labelCalls).To(Equal(1))
		Expect(forgeInst.comments).To(HaveLen(1))
		Expect(vmClosedFirst).To(BeTrue(), "the VM goes back to the pool before the forge is asked for anything")
		Expect(pendingClones()).To(BeEmpty())
		deliveries, err := sessionMgr.ListDeliveries(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(deliveries).To(BeEmpty())
	})

	It("finds a pull request whose create was answered with an error, and opens no second one", func() {
		forgeInst.prErrs = []error{&forge.APIError{Op: "create PR", StatusCode: 502, Body: "bad gateway"}}
		forgeInst.prLandsOnErr = true

		id := start(newService(0))
		Eventually(stateOf(id), 5*time.Second).Should(Equal(session.StateCompleted))

		Expect(forgeInst.prCalls).To(Equal(1))
		Expect(forgeInst.findCalls).To(BeNumerically(">=", 1))
		Expect(sessionOf(id)().PullRequestURL).To(Equal("https://github.com/test/repo/pull/1"))
		Expect(forgeInst.lastLabels).To(Equal([]string{"kvarn"}))
	})

	It("does not post the comment twice when a post landed but failed", func() {
		forgeInst.commentErrs = []error{&forge.APIError{Op: "post comment", StatusCode: 502, Body: "bad gateway"}}
		forgeInst.commentLandsOnErr = true

		id := start(newService(0))
		Eventually(stateOf(id), 5*time.Second).Should(Equal(session.StateCompleted))

		Expect(forgeInst.commentCalls).To(Equal(1))
		Expect(forgeInst.hasCommentCalls).To(Equal(1))
		Expect(forgeInst.comments).To(HaveLen(1))
		Expect(forgeInst.comments[0]).To(HaveSuffix("<!-- kvarn:delivery " + id + " -->"))
	})

	It("completes the run when the forge refuses the work log comment", func() {
		forgeInst.commentErr = &forge.APIError{Op: "post comment", StatusCode: 422, Body: "body is too long"}

		id := start(newService(0))
		Eventually(stateOf(id), 5*time.Second).Should(Equal(session.StateCompleted))
		Expect(forgeInst.commentCalls).To(Equal(1))
	})

	It("fails the run once the retry window has run out", func() {
		mockScm.pushErr = unreachable

		id := start(newService(100 * time.Millisecond))
		Eventually(stateOf(id), 5*time.Second).Should(Equal(session.StateFailed))

		sess := sessionOf(id)()
		Expect(sess.Error).To(ContainSubstring("Could not resolve host"))
		Expect(sess.Error).To(MatchRegexp(`gave up after \d+ attempts`))
		Expect(forgeInst.prCalls).To(BeZero())
		Expect(pendingClones()).To(BeEmpty())
	})

	It("cancels a delivery that waits for its next attempt", func() {
		DeferCleanup(orchestrator.SetDeliveryBackoffForTest(time.Hour))
		mockScm.pushErr = unreachable
		svc := newService(0)

		id := start(svc)
		Eventually(func() string {
			if s := sessionOf(id)(); s != nil {
				return s.Message
			}
			return ""
		}, 5*time.Second).Should(ContainSubstring("retrying in"))
		Expect(stateOf(id)()).To(Equal(session.StateSubmitting))

		_, err := svc.CancelJob(context.Background(), &v1.CancelJobRequest{SessionId: id, Reason: "not needed"})
		Expect(err).NotTo(HaveOccurred())

		Expect(stateOf(id)()).To(Equal(session.StateCancelled))
		Expect(pendingClones()).To(BeEmpty())
		deliveries, err := sessionMgr.ListDeliveries(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(deliveries).To(BeEmpty())
	})

	It("resumes a waiting delivery in the next process", func() {
		DeferCleanup(orchestrator.SetDeliveryBackoffForTest(300 * time.Millisecond))
		mockScm.pushErr = unreachable
		first := newService(0)

		id := start(first)
		Eventually(func() string {
			if s := sessionOf(id)(); s != nil {
				return s.Message
			}
			return ""
		}, 5*time.Second).Should(ContainSubstring("retrying in"))

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		first.Shutdown(ctx)
		cancel()

		deliveries, err := sessionMgr.ListDeliveries(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(deliveries).To(HaveLen(1), "a delivery waiting at shutdown stays stored")
		Expect(deliveries[0].Attempts).To(Equal(1))
		Expect(stateOf(id)()).To(Equal(session.StateSubmitting))

		mockScm.mu.Lock()
		mockScm.pushErr = nil
		mockScm.mu.Unlock()
		newService(0)

		Eventually(stateOf(id), 5*time.Second).Should(Equal(session.StateCompleted))
		Expect(sessionOf(id)().PullRequestURL).To(Equal("https://github.com/test/repo/pull/1"))
		Expect(forgeInst.prCalls).To(Equal(1))
		Expect(pendingClones()).To(BeEmpty())
	})
})
