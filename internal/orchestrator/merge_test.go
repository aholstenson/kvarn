package orchestrator_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"connectrpc.com/connect"
	llms "github.com/aholstenson/llms-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/agent/coding"
	"github.com/aholstenson/kvarn/internal/config/credential"
	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/forge"
	"github.com/aholstenson/kvarn/internal/orchestrator"
	"github.com/aholstenson/kvarn/internal/runner"
	"github.com/aholstenson/kvarn/internal/sandbox"
	gitscm "github.com/aholstenson/kvarn/internal/scm/git"
	"github.com/aholstenson/kvarn/internal/session"
	"github.com/aholstenson/kvarn/internal/vm"
)

// gitIn runs a git command in dir and fails the spec if it does not succeed.
func gitIn(dir string, args ...string) string {
	GinkgoHelper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// mergingAgent drives the merge tools the way the model would: merge, resolve
// what conflicted, finish, and optionally keep working afterwards.
//
// It builds the toolkit from the agent context rather than calling the sandbox
// helpers directly, so these specs exercise the same path a real run takes.
type mergingAgent struct {
	// resolve is written into the workspace after the merge, standing in for the
	// agent editing the conflicted files.
	resolve map[string]string
	// finish decides whether the merge is concluded. Leaving it false is the
	// half-resolved tree the run must refuse to deliver.
	finish bool
	// trailing is written after the merge is finished, and so belongs in a
	// second commit.
	trailing map[string]string

	mu          sync.Mutex
	sawTools    bool
	mergeTarget *agent.MergeTarget
	toolErr     error
}

func (a *mergingAgent) Start(_ context.Context, agentCtx *agent.Context) (agent.Conversation, error) {
	return &mergingConversation{a: a, agentCtx: agentCtx}, nil
}

func (a *mergingAgent) target() *agent.MergeTarget {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mergeTarget
}

func (a *mergingAgent) offeredTools() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sawTools
}

type mergingConversation struct {
	a        *mergingAgent
	agentCtx *agent.Context
}

func (c *mergingConversation) Run(ctx context.Context, _ string) (string, error) {
	toolkit := coding.NewCodingToolkitWithOpts(coding.CodingToolkitOpts{
		Runner:       c.agentCtx.Runner,
		WorkingDir:   c.agentCtx.WorkingDir,
		SessionID:    c.agentCtx.SessionID,
		HeadBranch:   c.agentCtx.Branch,
		MergeTarget:  c.agentCtx.MergeTarget,
		Checkpointer: c.agentCtx.Checkpointer,
	})
	tools := make(map[string]llms.ToolDef)
	for _, t := range toolkit.Tools() {
		tools[t.Name()] = t
	}

	c.a.mu.Lock()
	c.a.mergeTarget = c.agentCtx.MergeTarget
	_, c.a.sawTools = tools["merge_branch"]
	c.a.mu.Unlock()

	if _, ok := tools["merge_branch"]; !ok {
		return "", nil
	}

	if _, err := tools["merge_branch"].Execute(ctx, &coding.MergeBranchInput{}); err != nil {
		return "", c.record(err)
	}
	if err := c.write(c.a.resolve); err != nil {
		return "", err
	}
	if !c.a.finish {
		return "", nil
	}
	if _, err := tools["finish_merge"].Execute(ctx, &coding.FinishMergeInput{
		Notes: "kept both sides",
	}); err != nil {
		return "", c.record(err)
	}
	return "", c.write(c.a.trailing)
}

// record keeps a tool failure for the spec to inspect while still failing the
// run, so a broken merge does not show up only as an opaque agent error.
func (c *mergingConversation) record(err error) error {
	c.a.mu.Lock()
	c.a.toolErr = err
	c.a.mu.Unlock()
	return err
}

func (c *mergingConversation) write(files map[string]string) error {
	for name, body := range files {
		path := filepath.Join(c.agentCtx.WorkingDir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (c *mergingConversation) Summarize(_ context.Context) (*agent.Result, error) {
	return &agent.Result{
		Title:       "Resolve the merge conflicts",
		Description: "Merged the base branch and resolved the conflict in shared.txt.",
	}, nil
}

func (c *mergingConversation) Close() error { return nil }

var _ = Describe("Merging the base branch into a pull request", func() {
	var (
		client        kvarnv1connect.OrchestratorServiceClient
		server        *http.Server
		mockForgeInst *mockForge
		sessionMgr    session.Manager
		listener      net.Listener
		tmpDir        string
		bareDir       string
		testAgent     *mergingAgent
		featureTip    string
		baseTip       string
	)

	BeforeEach(func() {
		sessionMgr = session.NewManager(session.NewMemStore())
		testAgent = &mergingAgent{}

		var err error
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())

		tmpDir, err = os.MkdirTemp("", "merge-job-test-*")
		Expect(err).NotTo(HaveOccurred())

		// A pull request that has fallen behind: both branches changed
		// shared.txt, so a merge conflicts.
		bareDir = filepath.Join(tmpDir, "repo.git")
		gitIn(tmpDir, "init", "--bare", "-b", "main", bareDir)

		seed := filepath.Join(tmpDir, "seed")
		gitIn(tmpDir, "clone", bareDir, seed)
		gitIn(seed, "config", "user.email", "test@test.com")
		gitIn(seed, "config", "user.name", "Test")

		Expect(os.WriteFile(filepath.Join(seed, "shared.txt"), []byte("one\n"), 0o644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(seed, "kvarn.yml"), []byte("version: 1\n"), 0o644)).To(Succeed())
		gitIn(seed, "add", "-A")
		gitIn(seed, "commit", "-m", "initial")
		gitIn(seed, "push", "origin", "main")

		gitIn(seed, "checkout", "-b", "feature")
		Expect(os.WriteFile(filepath.Join(seed, "shared.txt"), []byte("from feature\n"), 0o644)).To(Succeed())
		gitIn(seed, "add", "-A")
		gitIn(seed, "commit", "-m", "feature work")
		gitIn(seed, "push", "origin", "feature")
		featureTip = gitIn(seed, "rev-parse", "HEAD")

		gitIn(seed, "checkout", "main")
		Expect(os.WriteFile(filepath.Join(seed, "shared.txt"), []byte("from base\n"), 0o644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(seed, "base-only.txt"), []byte("base\n"), 0o644)).To(Succeed())
		gitIn(seed, "add", "-A")
		gitIn(seed, "commit", "-m", "base moves on")
		gitIn(seed, "push", "origin", "main")
		baseTip = gitIn(seed, "rev-parse", "HEAD")

		projStore := &memProjectStore{
			projects: map[string]*project.Project{
				"test-project": {
					Name:          "test-project",
					RepoURL:       bareDir,
					DefaultBranch: "main",
					Forge:         "test-forge",
				},
			},
		}
		forgeConfigStore := &memForgeConfigStore{
			configs: map[string]*forgeconfig.ForgeConfig{
				"test-forge": {Name: "test-forge", Type: "mock", BranchPrefix: "kvarn", Credential: "test-cred"},
			},
		}
		// Delivery needs a token to be possible at all; the local repository the
		// push goes to never asks for it.
		credStore := &memCredentialStore{
			creds: map[string]*credential.Credential{
				"test-cred": {Name: "test-cred", Config: map[string]string{"token": "ghp_fake"}},
			},
		}

		// The workspace is a copy of the job clone, so it carries the base branch
		// the orchestrator fetched into it — the guest sees what the transfer
		// would have shipped.
		factory := func(_ context.Context, opts sandbox.Opts) (orchestrator.Sandbox, error) {
			wsDir, err := os.MkdirTemp("", "merge-ws-*")
			if err != nil {
				return nil, err
			}
			cp := exec.Command("cp", "-a", opts.SourceDir+"/.", wsDir)
			if out, err := cp.CombinedOutput(); err != nil {
				return nil, fmt.Errorf("copy source: %s: %w", out, err)
			}

			h := runner.NewUnprivilegedHandler()
			proxy := &localRunnerProxy{handler: h}
			sessResp, err := proxy.CreateSession(context.Background(), &v1.CreateSessionRequest{WorkingDir: wsDir})
			if err != nil {
				return nil, err
			}
			return newTestSandbox(proxy, sessResp.SessionId, wsDir), nil
		}

		// A real git implementation, because what this is about is the shape of
		// the commits that reach the forge.
		mockForgeInst = &mockForge{
			scmImpl: &gitscm.Git{},
			pullRequests: map[string]*forge.PullRequestDetails{
				"7": {
					Ref:        "7",
					State:      "open",
					HeadBranch: "feature",
					HeadSHA:    featureTip,
					HeadRepo:   "owner/repo",
					BaseBranch: "main",
					BaseRepo:   "owner/repo",
					Title:      "Add a feature",
					URL:        "https://github.com/owner/repo/pull/7",
				},
			},
		}

		svc := orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{
			CreateOpts:       vm.CreateOpts{},
			ProjectStore:     projStore,
			CredentialStore:  credStore,
			ForgeConfigStore: forgeConfigStore,
			ForgeTypes:       map[string]forge.Forge{"mock": mockForgeInst},
			SessionMgr:       sessionMgr,
			Agent:            testAgent,
			SandboxFactory:   factory,
		})

		mux := http.NewServeMux()
		path, handler := kvarnv1connect.NewOrchestratorServiceHandler(svc)
		mux.Handle(path, handler)
		bridgePath, bridgeHandler := kvarnv1connect.NewBridgeServiceHandler(svc.BridgeHandler())
		mux.Handle(bridgePath, bridgeHandler)

		server = &http.Server{Handler: mux}
		go server.Serve(listener)

		client = kvarnv1connect.NewOrchestratorServiceClient(
			http.DefaultClient, fmt.Sprintf("http://%s", listener.Addr().String()))
	})

	AfterEach(func() {
		server.Close()
		os.RemoveAll(tmpDir)
	})

	submit := func(mode string) string {
		GinkgoHelper()
		resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project:   "test-project",
			Prompt:    "merge the base branch and fix the conflicts",
			Mode:      mode,
			StartFrom: &v1.StartJobRequest_PrRef{PrRef: "7"},
		}))
		Expect(err).NotTo(HaveOccurred())
		return resp.Msg.SessionId
	}

	// A run here does real git work — a clone, a fetch, a merge, a push — so it
	// is given room rather than the default one second.
	settlesAs := func(sid, state string) {
		GinkgoHelper()
		Eventually(func() string {
			sess, err := sessionMgr.Get(context.Background(), sid)
			if err != nil {
				return ""
			}
			return string(sess.State)
		}, "30s", "50ms").Should(Equal(state))
	}

	// pushedTip is where the pull request's branch ends up on the forge.
	pushedTip := func() string { return gitIn(bareDir, "rev-parse", "refs/heads/feature") }

	It("pushes the merge as a commit with both parents", func() {
		testAgent.resolve = map[string]string{"shared.txt": "from both\n"}
		testAgent.finish = true

		sid := submit("resolve-conflicts")
		settlesAs(sid, "completed")

		Expect(testAgent.offeredTools()).To(BeTrue())
		Expect(testAgent.target()).To(Equal(&agent.MergeTarget{Branch: "main", SHA: baseTip}))

		// The pull request now has exactly one new commit, and it is a merge of
		// the base tip the run was given.
		tip := pushedTip()
		Expect(gitIn(bareDir, "rev-list", "--parents", "-n", "1", tip)).
			To(Equal(strings.Join([]string{tip, featureTip, baseTip}, " ")))
		Expect(gitIn(bareDir, "log", "-1", "--format=%s", tip)).
			To(Equal("Merge branch 'main' into 'feature'"))

		// And it carries the resolution plus what the base branch brought in.
		Expect(gitIn(bareDir, "show", tip+":shared.txt")).To(Equal("from both"))
		Expect(gitIn(bareDir, "show", tip+":base-only.txt")).To(Equal("base"))
	})

	It("pushes the merge and the work that followed it as two commits", func() {
		testAgent.resolve = map[string]string{"shared.txt": "from both\n"}
		testAgent.finish = true
		testAgent.trailing = map[string]string{"fix.txt": "after the merge\n"}

		sid := submit("resolve-conflicts")
		settlesAs(sid, "completed")

		tip := pushedTip()
		Expect(gitIn(bareDir, "log", "-1", "--format=%s", tip)).To(Equal("Resolve the merge conflicts"))
		Expect(gitIn(bareDir, "show", tip+":fix.txt")).To(Equal("after the merge"))

		// The merge sits underneath it, still with both parents — the trailing
		// commit did not absorb it.
		merge := gitIn(bareDir, "rev-parse", tip+"^")
		Expect(gitIn(bareDir, "rev-list", "--parents", "-n", "1", merge)).
			To(Equal(strings.Join([]string{merge, featureTip, baseTip}, " ")))
		Expect(gitIn(bareDir, "rev-parse", tip+"^^")).To(Equal(featureTip))
	})

	It("pushes the merge alone when the run changed nothing else", func() {
		// The whole task was the merge, so the trailing diff is empty — which
		// used to read as "nothing to submit".
		testAgent.resolve = map[string]string{"shared.txt": "from both\n"}
		testAgent.finish = true

		sid := submit("resolve-conflicts")
		settlesAs(sid, "completed")

		tip := pushedTip()
		Expect(gitIn(bareDir, "rev-parse", tip+"^")).To(Equal(featureTip))
		Expect(mockForgeInst.commentCalls).To(Equal(1))
	})

	It("fails the run and pushes nothing when the merge is left unfinished", func() {
		testAgent.resolve = map[string]string{"shared.txt": "from both\n"}
		testAgent.finish = false

		sid := submit("resolve-conflicts")
		settlesAs(sid, "failed")

		sess, err := sessionMgr.Get(context.Background(), sid)
		Expect(err).NotTo(HaveOccurred())
		Expect(sess.Error).To(ContainSubstring("never finished"))

		// The pull request is exactly where it was.
		Expect(pushedTip()).To(Equal(featureTip))
	})

	It("offers no merge tools to a read-only run", func() {
		sid := submit("review")
		settlesAs(sid, "completed")

		Expect(testAgent.offeredTools()).To(BeFalse())
		Expect(testAgent.target()).To(BeNil())
		Expect(pushedTip()).To(Equal(featureTip))
	})
})
