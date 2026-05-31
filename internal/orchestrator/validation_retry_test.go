package orchestrator_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/config/credential"
	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/forge"
	"github.com/aholstenson/kvarn/internal/orchestrator"
	"github.com/aholstenson/kvarn/internal/runner"
	"github.com/aholstenson/kvarn/internal/sandbox"
	"github.com/aholstenson/kvarn/internal/session"
	"github.com/aholstenson/kvarn/internal/vm"
)

// retryAgent records per-attempt followup strings and lets the test mutate
// the workspace via a Run callback so a kvarn.yml validation step can switch
// from failing to passing across attempts.
type retryAgent struct {
	mu             sync.Mutex
	followups      []string
	summarizeCalls int
	onRun          func(attempt int, workingDir string, followup string) error
}

func (a *retryAgent) Start(_ context.Context, agentCtx *agent.Context) (agent.Conversation, error) {
	return &retryConversation{a: a, agentCtx: agentCtx}, nil
}

type retryConversation struct {
	a        *retryAgent
	agentCtx *agent.Context
	attempts int
}

func (c *retryConversation) Run(_ context.Context, followup string) (string, error) {
	c.a.mu.Lock()
	c.a.followups = append(c.a.followups, followup)
	attempt := c.attempts
	c.attempts++
	c.a.mu.Unlock()
	if c.a.onRun != nil {
		if err := c.a.onRun(attempt, c.agentCtx.WorkingDir, followup); err != nil {
			return "", err
		}
	}
	return "", nil
}

func (c *retryConversation) Summarize(_ context.Context) (*agent.Result, error) {
	c.a.mu.Lock()
	c.a.summarizeCalls++
	c.a.mu.Unlock()
	return &agent.Result{
		Title:       "Apply fix",
		Description: "fixed it",
	}, nil
}

func (c *retryConversation) Close() error { return nil }

var _ = Describe("StartJob validation retry", func() {
	var (
		client        kvarnv1connect.OrchestratorServiceClient
		server        *http.Server
		mockScm       *mockSCM
		mockForgeInst *mockForge
		sessionMgr    *session.MemoryManager
		listener      net.Listener
		tmpDir        string
		ag            *retryAgent
		projStore     *memProjectStore
	)

	BeforeEach(func() {
		mockScm = &mockSCM{}
		sessionMgr = session.NewMemoryManager()
		ag = &retryAgent{}

		var err error
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())

		tmpDir, err = os.MkdirTemp("", "retry-test-*")
		Expect(err).NotTo(HaveOccurred())

		// Default: no per-job retry cap override; resolver uses builtin
		// MaxValidationRetries=3.
		projStore = &memProjectStore{
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

		factory := func(_ context.Context, opts sandbox.Opts) (orchestrator.Sandbox, error) {
			wsDir, err := os.MkdirTemp("", "retry-ws-*")
			if err != nil {
				return nil, err
			}
			if opts.SourceDir != "" {
				cp := exec.Command("cp", "-a", opts.SourceDir+"/.", wsDir)
				if out, err := cp.CombinedOutput(); err != nil {
					return nil, fmt.Errorf("copy source: %s: %w", out, err)
				}
			}
			if _, err := os.Stat(filepath.Join(wsDir, ".git")); os.IsNotExist(err) {
				Expect(exec.Command("git", "init", wsDir).Run()).To(Succeed())
				Expect(os.WriteFile(filepath.Join(wsDir, "seed.txt"), []byte("seed\n"), 0644)).To(Succeed())
				ac := exec.Command("git", "add", ".")
				ac.Dir = wsDir
				Expect(ac.Run()).To(Succeed())
				cc := exec.Command("git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")
				cc.Dir = wsDir
				Expect(cc.Run()).To(Succeed())
			}
			h := runner.NewUnprivilegedHandler()
			proxy := &localRunnerProxy{handler: h}
			sessResp, err := proxy.CreateSession(context.Background(), &v1.CreateSessionRequest{WorkingDir: wsDir})
			if err != nil {
				return nil, err
			}
			return &testSandbox{
				runner:         proxy,
				shellSessionID: sessResp.SessionId,
				workingDir:     wsDir,
			}, nil
		}

		mockForgeInst = &mockForge{scmImpl: mockScm}

		svc := orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{
			CreateOpts:       vm.CreateOpts{},
			ProjectStore:     projStore,
			CredentialStore:  credStore,
			ForgeConfigStore: forgeConfigStore,
			ForgeTypes: map[string]forge.Forge{
				"mock": mockForgeInst,
			},
			SessionMgr:     sessionMgr,
			Agent:          ag,
			SandboxFactory: factory,
		})

		mux := http.NewServeMux()
		path, handler := kvarnv1connect.NewOrchestratorServiceHandler(svc)
		mux.Handle(path, handler)
		bridgePath, bridgeHandler := kvarnv1connect.NewBridgeServiceHandler(svc.BridgeHandler())
		mux.Handle(bridgePath, bridgeHandler)
		server = &http.Server{Handler: mux}
		go server.Serve(listener)

		client = kvarnv1connect.NewOrchestratorServiceClient(
			http.DefaultClient,
			fmt.Sprintf("http://%s", listener.Addr().String()),
		)
	})

	AfterEach(func() {
		server.Close()
		os.RemoveAll(tmpDir)
	})

	stateOf := func(sid string) func() string {
		return func() string {
			s, err := sessionMgr.Get(context.Background(), sid)
			if err != nil {
				return ""
			}
			return string(s.State)
		}
	}

	It("retries the agent when required validation fails and completes when it passes", func() {
		// Validation grep for "fixed" in seed.txt. The agent writes to the
		// tracked file on its second attempt (i.e. the first retry) — so
		// ChangedFiles sees the diff and the PR submission path runs.
		mockScm.files = map[string][]byte{
			"kvarn.yml": []byte("validation:\n  required:\n    - name: Marker\n      run: grep -q fixed seed.txt\n"),
		}
		ag.onRun = func(attempt int, workingDir string, _ string) error {
			if attempt == 1 {
				return os.WriteFile(filepath.Join(workingDir, "seed.txt"), []byte("seed\nfixed\n"), 0644)
			}
			return nil
		}

		resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "do the thing",
		}))
		Expect(err).NotTo(HaveOccurred())

		Eventually(stateOf(resp.Msg.SessionId)).Should(Equal("completed"))

		ag.mu.Lock()
		defer ag.mu.Unlock()
		Expect(ag.followups).To(HaveLen(2))
		Expect(ag.followups[0]).To(BeEmpty())
		Expect(ag.followups[1]).To(ContainSubstring("Step Marker exited 1"))
		Expect(ag.summarizeCalls).To(Equal(1))
		Expect(mockForgeInst.prCalls).To(Equal(1))
	})

	It("fails the session when the retry cap is exhausted", func() {
		// Project caps retries at 1 (one additional attempt after the first).
		// Validation always fails.
		one := 1
		projStore.projects["test-project"].MaxValidationRetries = &one
		mockScm.files = map[string][]byte{
			"kvarn.yml": []byte("validation:\n  required:\n    - name: AlwaysFails\n      run: exit 1\n"),
		}

		resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "do the thing",
		}))
		Expect(err).NotTo(HaveOccurred())

		Eventually(stateOf(resp.Msg.SessionId)).Should(Equal("failed"))

		s, err := sessionMgr.Get(context.Background(), resp.Msg.SessionId)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.Error).To(ContainSubstring("after 2 attempts"))

		ag.mu.Lock()
		defer ag.mu.Unlock()
		Expect(ag.followups).To(HaveLen(2))
		Expect(ag.summarizeCalls).To(Equal(0))
		Expect(mockForgeInst.prCalls).To(Equal(0))
	})

	It("preserves single-attempt behaviour when MaxValidationRetries is 0", func() {
		zero := 0
		projStore.projects["test-project"].MaxValidationRetries = &zero
		mockScm.files = map[string][]byte{
			"kvarn.yml": []byte("validation:\n  required:\n    - name: AlwaysFails\n      run: exit 1\n"),
		}

		resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "do the thing",
		}))
		Expect(err).NotTo(HaveOccurred())

		Eventually(stateOf(resp.Msg.SessionId)).Should(Equal("failed"))

		ag.mu.Lock()
		defer ag.mu.Unlock()
		Expect(ag.followups).To(HaveLen(1))
		Expect(ag.followups[0]).To(BeEmpty())
		Expect(ag.summarizeCalls).To(Equal(0))
	})
})
