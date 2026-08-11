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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// answeringAgent is a read-only agent: it writes nothing and its turn returns
// the answer, which is what a mode that delivers a comment has to work from.
type answeringAgent struct {
	answer string

	mu       sync.Mutex
	prompt   string
	readOnly bool
}

func (a *answeringAgent) lastPrompt() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.prompt
}

func (a *answeringAgent) sawReadOnlyMode() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.readOnly
}

func (a *answeringAgent) Start(_ context.Context, agentCtx *agent.Context) (agent.Conversation, error) {
	a.mu.Lock()
	a.prompt = agentCtx.Prompt
	a.readOnly = agentCtx.Mode != nil && !agentCtx.Mode.WritesChanges()
	a.mu.Unlock()
	return &answeringConversation{answer: a.answer}, nil
}

type answeringConversation struct{ answer string }

func (c *answeringConversation) Run(context.Context, string) (string, error) { return c.answer, nil }

func (c *answeringConversation) Summarize(context.Context) (*agent.Result, error) {
	return nil, fmt.Errorf("a read-only run must not be summarized")
}

func (c *answeringConversation) Close() error { return nil }

var _ = Describe("Custom agent modes", func() {
	var (
		client        kvarnv1connect.OrchestratorServiceClient
		server        *http.Server
		mockScm       *mockSCM
		mockForgeInst *mockForge
		sessionMgr    session.Manager
		listener      net.Listener
		tmpDir        string
		testAgent     *answeringAgent
		// runAgent is what the service runs with. It is the read-only answering
		// agent unless a test needs one that writes.
		runAgent agent.Agent
		// kvarnYML is injected into the clone, so the modes a run resolves
		// against are the ones this repository defines.
		kvarnYML string
	)

	openPR := func() *forge.PullRequestDetails {
		return &forge.PullRequestDetails{
			Ref:        "42",
			State:      "open",
			HeadBranch: "contributor/add-a-helper",
			HeadSHA:    "abc123",
			HeadRepo:   "owner/repo",
			BaseBranch: "master",
			BaseRepo:   "owner/repo",
			Title:      "Add a helper",
			Body:       "Adds a small helper.",
			URL:        "https://github.com/owner/repo/pull/42",
		}
	}

	// startService boots an orchestrator whose clones carry the kvarnYML set
	// by the test. It runs at the end of each BeforeEach chain rather than in
	// one, so a test can choose its repository config first.
	startService := func() {
		mockScm.files = map[string][]byte{"kvarn.yml": []byte(kvarnYML)}

		factory := func(_ context.Context, opts sandbox.Opts) (orchestrator.Sandbox, error) {
			wsDir, err := os.MkdirTemp("", "custommode-ws-*")
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
				},
			}},
			CredentialStore: &memCredentialStore{creds: map[string]*credential.Credential{
				"test-cred": {Name: "test-cred", Config: map[string]string{"token": "ghp_fake"}},
			}},
			ForgeConfigStore: &memForgeConfigStore{configs: map[string]*forgeconfig.ForgeConfig{
				"test-forge": {Name: "test-forge", Type: "mock", Credential: "test-cred"},
			}},
			ForgeTypes:     map[string]forge.Forge{"mock": mockForgeInst},
			SessionMgr:     sessionMgr,
			Agent:          runAgent,
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
			http.DefaultClient, fmt.Sprintf("http://%s", listener.Addr().String()))
	}

	BeforeEach(func() {
		mockScm = &mockSCM{}
		sessionMgr = session.NewManager(session.NewMemStore())
		testAgent = &answeringAgent{answer: "Approve with comments: the helper needs a test."}
		runAgent = testAgent
		mockForgeInst = &mockForge{
			scmImpl:      mockScm,
			pullRequests: map[string]*forge.PullRequestDetails{"42": openPR()},
			diff:         "diff --git a/helper.go b/helper.go\n+func helper() {}\n",
		}

		var err error
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		tmpDir, err = os.MkdirTemp("", "custommode-test-*")
		Expect(err).NotTo(HaveOccurred())

		// The default repository defines a read-only pull-request review that
		// delivers its verdict as a comment.
		kvarnYML = `
modes:
  review-pr:
    description: Review an open pull request.
    extends: review
    start: pull-request
    deliver:
      - pr-comment
    context:
      - pr-metadata
      - pr-diff
    prompt: |
      Hold the change to the house style.
`
	})

	AfterEach(func() {
		if server != nil {
			server.Close()
		}
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

	Describe("a repository-defined read-only mode delivering a comment", func() {
		It("posts the result on the pull request without pushing anything", func() {
			startService()
			resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project:   "test-project",
				Prompt:    "review this",
				Mode:      "review-pr",
				StartFrom: &v1.StartJobRequest_PrRef{PrRef: "42"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Eventually(stateOf(resp.Msg.SessionId)).Should(Equal("completed"))

			Expect(testAgent.sawReadOnlyMode()).To(BeTrue())

			mockScm.mu.Lock()
			pushCalls := mockScm.pushCalls
			mockScm.mu.Unlock()
			Expect(pushCalls).To(BeZero(), "a read-only mode commits nothing")
			Expect(mockForgeInst.prCalls).To(BeZero())

			Expect(mockForgeInst.commentCalls).To(Equal(1))
			Expect(mockForgeInst.lastCommentOpts.PRRef).To(Equal("42"))
			Expect(mockForgeInst.lastCommentOpts.Body).To(HavePrefix("## Result\n\nApprove with comments"))
			Expect(mockForgeInst.lastCommentOpts.Body).To(ContainSubstring("## Task\n\n> review this"))
		})

		It("hands the agent the context blocks the mode asked for and no others", func() {
			startService()
			resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project:   "test-project",
				Prompt:    "review this",
				Mode:      "review-pr",
				StartFrom: &v1.StartJobRequest_PrRef{PrRef: "42"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Eventually(stateOf(resp.Msg.SessionId)).Should(Equal("completed"))

			prompt := testAgent.lastPrompt()
			Expect(prompt).To(ContainSubstring("## Current pull request"))
			Expect(prompt).To(ContainSubstring("## Diff"))
			Expect(prompt).To(ContainSubstring("## Task\n\nreview this"))
			Expect(prompt).NotTo(ContainSubstring("## Feedback to address"))
		})

		It("records the result so it can be read back after the run", func() {
			startService()
			resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project:   "test-project",
				Prompt:    "review this",
				Mode:      "review-pr",
				StartFrom: &v1.StartJobRequest_PrRef{PrRef: "42"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Eventually(stateOf(resp.Msg.SessionId)).Should(Equal("completed"))

			got, err := client.GetSessionResult(context.Background(),
				connect.NewRequest(&v1.GetSessionResultRequest{SessionId: resp.Msg.SessionId}))
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Msg.State).To(Equal("completed"))
			Expect(got.Msg.Result).To(ContainSubstring("Approve with comments"))
		})

		It("fails the run when the comment cannot be posted", func() {
			mockForgeInst.commentErr = fmt.Errorf("forge unavailable")
			startService()
			resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project:   "test-project",
				Prompt:    "review this",
				Mode:      "review-pr",
				StartFrom: &v1.StartJobRequest_PrRef{PrRef: "42"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Eventually(stateOf(resp.Msg.SessionId)).Should(Equal("failed"))

			sess, err := sessionMgr.Get(context.Background(), resp.Msg.SessionId)
			Expect(err).NotTo(HaveOccurred())
			Expect(sess.Error).To(ContainSubstring("post result comment"))
		})

		It("allows two concurrent runs on one pull request, since neither pushes", func() {
			startService()
			first, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project:   "test-project",
				Prompt:    "review this",
				Mode:      "review-pr",
				StartFrom: &v1.StartJobRequest_PrRef{PrRef: "42"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Eventually(stateOf(first.Msg.SessionId)).Should(Equal("completed"))

			_, err = client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project:   "test-project",
				Prompt:    "review it again",
				Mode:      "review-pr",
				StartFrom: &v1.StartJobRequest_PrRef{PrRef: "42"},
			}))
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("a read-only mode that requires validation", func() {
		BeforeEach(func() {
			kvarnYML = `
validation:
  required:
    - name: Tests
      run: exit 1

modes:
  test-pr:
    extends: review
    start: pull-request
    validation: require
    deliver:
      - pr-comment
`
		})

		It("runs the project's checks and fails the run on a red required step", func() {
			startService()
			resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project:   "test-project",
				Prompt:    "does this pass?",
				Mode:      "test-pr",
				StartFrom: &v1.StartJobRequest_PrRef{PrRef: "42"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Eventually(stateOf(resp.Msg.SessionId)).Should(Equal("failed"))

			sess, err := sessionMgr.Get(context.Background(), resp.Msg.SessionId)
			Expect(err).NotTo(HaveOccurred())
			Expect(sess.Error).To(ContainSubstring("required validation steps failed"))
		})

		It("posts the failing verdict before failing the run", func() {
			startService()
			resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project:   "test-project",
				Prompt:    "does this pass?",
				Mode:      "test-pr",
				StartFrom: &v1.StartJobRequest_PrRef{PrRef: "42"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Eventually(stateOf(resp.Msg.SessionId)).Should(Equal("failed"))

			Expect(mockForgeInst.commentCalls).To(Equal(1),
				"the verdict is what the mode exists to produce, so a red run still delivers it")
			Expect(mockForgeInst.lastCommentOpts.PRRef).To(Equal("42"))
			Expect(mockForgeInst.lastCommentOpts.Body).To(ContainSubstring("A required step failed"))
			Expect(mockForgeInst.lastCommentOpts.Body).To(ContainSubstring("Tests — failed (exit 1)"))

			got, err := client.GetSessionResult(context.Background(),
				connect.NewRequest(&v1.GetSessionResultRequest{SessionId: resp.Msg.SessionId}))
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Msg.Result).To(ContainSubstring("Approve with comments"))
		})

		It("runs path-scoped required steps, which a run with no diff of its own would otherwise skip", func() {
			kvarnYML = `
validation:
  required:
    - name: Tests
      run: exit 1
      paths:
        - "**/*.go"

modes:
  test-pr:
    extends: review
    start: pull-request
    validation: require
    deliver:
      - pr-comment
`
			startService()
			resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project:   "test-project",
				Prompt:    "does this pass?",
				Mode:      "test-pr",
				StartFrom: &v1.StartJobRequest_PrRef{PrRef: "42"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Eventually(stateOf(resp.Msg.SessionId)).Should(Equal("failed"),
				"gating on an empty diff would have skipped the step into a green result")
			Expect(mockForgeInst.lastCommentOpts.Body).To(ContainSubstring("Tests — failed (exit 1)"))
		})

		It("completes when the required steps pass", func() {
			kvarnYML = `
validation:
  required:
    - name: Tests
      run: echo pass

modes:
  test-pr:
    extends: review
    start: pull-request
    validation: require
    deliver:
      - pr-comment
`
			startService()
			resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project:   "test-project",
				Prompt:    "does this pass?",
				Mode:      "test-pr",
				StartFrom: &v1.StartJobRequest_PrRef{PrRef: "42"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Eventually(stateOf(resp.Msg.SessionId)).Should(Equal("completed"))
			Expect(mockForgeInst.commentCalls).To(Equal(1))
		})
	})

	Describe("a mode that opens pull requests, run against an existing one", func() {
		BeforeEach(func() {
			kvarnYML = ""
			runAgent = &scriptedAgent{
				title:       "Add the missing test",
				description: "Adds the test the review asked for.",
				fileName:    "README.md",
				fileBody:    "# Test\n\nnow with a test\n",
			}
		})

		It("commits onto the pull request rather than delivering nothing", func() {
			startService()
			resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project:   "test-project",
				Prompt:    "add the test",
				Mode:      "implement",
				StartFrom: &v1.StartJobRequest_PrRef{PrRef: "42"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Eventually(stateOf(resp.Msg.SessionId)).Should(Equal("completed"))

			mockScm.mu.Lock()
			pushCalls := mockScm.pushCalls
			pushedBranch := mockScm.lastPushOpts.Branch
			mockScm.mu.Unlock()
			Expect(pushCalls).To(Equal(1), "the work has to land somewhere")
			Expect(pushedBranch).To(Equal("contributor/add-a-helper"), "onto the pull request's own head branch")
			Expect(mockForgeInst.prCalls).To(BeZero(), "naming a pull request did not ask for a second one")
			Expect(mockForgeInst.commentCalls).To(Equal(1))
		})
	})

	Describe("an inline mode definition", func() {
		// inlineReview is the same review-pr mode, supplied with the request
		// instead of coming out of the repository.
		inlineReview := func() *v1.ModeSpec {
			return &v1.ModeSpec{
				Name:    "review-inline",
				Extends: "review",
				Start:   "pull-request",
				Deliver: []string{"pr-comment"},
				Context: []string{"pr-metadata", "pr-diff"},
				Prompt:  "Hold the change to the house style.",
			}
		}

		It("survives submission, the backlog and dispatch, and produces the same delivery", func() {
			kvarnYML = "" // no repository modes at all
			startService()

			resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project:   "test-project",
				Prompt:    "review this",
				ModeSpec:  inlineReview(),
				StartFrom: &v1.StartJobRequest_PrRef{PrRef: "42"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Eventually(stateOf(resp.Msg.SessionId)).Should(Equal("completed"))

			sess, err := sessionMgr.Get(context.Background(), resp.Msg.SessionId)
			Expect(err).NotTo(HaveOccurred())
			Expect(sess.Mode).To(Equal("review-inline"))
			Expect(sess.ModeSpecJSON).To(ContainSubstring(`"extends":"review"`))

			Expect(mockForgeInst.commentCalls).To(Equal(1))
			Expect(mockForgeInst.lastCommentOpts.Body).To(ContainSubstring("Approve with comments"))
			Expect(testAgent.lastPrompt()).To(ContainSubstring("## Diff"))
		})

		It("is rejected when its axes do not make sense", func() {
			startService()
			_, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project: "test-project",
				Prompt:  "review this",
				ModeSpec: &v1.ModeSpec{
					Name:      "broken",
					Extends:   "review",
					Workspace: "read-only",
					Deliver:   []string{"new-pull-request"},
				},
			}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("read-write"))
		})

		It("is rejected when it starts somewhere the mode does not accept", func() {
			startService()
			_, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project:  "test-project",
				Prompt:   "review this",
				ModeSpec: inlineReview(),
			}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("existing pull request"))
		})

		It("inherits the start point of the mode it extends rather than accepting a branch", func() {
			startService()
			_, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project:  "test-project",
				Prompt:   "revise this",
				ModeSpec: &v1.ModeSpec{Name: "revise", Extends: "feedback"},
			}))
			Expect(err).To(HaveOccurred(), "a follow-up commit has nowhere to land without a pull request")
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("existing pull request"))
		})

		It("is rejected when it comments on a pull request the run would not have", func() {
			startService()
			_, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project: "test-project",
				Prompt:  "review this",
				ModeSpec: &v1.ModeSpec{
					Name:    "verdict",
					Extends: "review",
					Deliver: []string{"pr-comment"},
				},
			}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("comment on a pull request"))
		})

		It("replays an idempotency key that carries the same definition", func() {
			startService()
			req := func() *connect.Request[v1.StartJobRequest] {
				return connect.NewRequest(&v1.StartJobRequest{
					Project:        "test-project",
					Prompt:         "review this",
					ModeSpec:       inlineReview(),
					StartFrom:      &v1.StartJobRequest_PrRef{PrRef: "42"},
					IdempotencyKey: "key-1",
				})
			}
			first, err := client.StartJob(context.Background(), req())
			Expect(err).NotTo(HaveOccurred())

			second, err := client.StartJob(context.Background(), req())
			Expect(err).NotTo(HaveOccurred())
			Expect(second.Msg.SessionId).To(Equal(first.Msg.SessionId))
			Expect(second.Msg.Duplicate).To(BeTrue())
		})

		It("refuses an idempotency key reused for a different definition", func() {
			startService()
			_, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project:        "test-project",
				Prompt:         "review this",
				ModeSpec:       inlineReview(),
				StartFrom:      &v1.StartJobRequest_PrRef{PrRef: "42"},
				IdempotencyKey: "key-1",
			}))
			Expect(err).NotTo(HaveOccurred())

			differing := inlineReview()
			differing.Prompt = "Something else entirely."
			_, err = client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project:        "test-project",
				Prompt:         "review this",
				ModeSpec:       differing,
				StartFrom:      &v1.StartJobRequest_PrRef{PrRef: "42"},
				IdempotencyKey: "key-1",
			}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeAlreadyExists))
		})

		It("carries the definition through a retry", func() {
			kvarnYML = ""
			startService()

			first, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project:   "test-project",
				Prompt:    "review this",
				ModeSpec:  inlineReview(),
				StartFrom: &v1.StartJobRequest_PrRef{PrRef: "42"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Eventually(stateOf(first.Msg.SessionId)).Should(Equal("completed"))

			retried, err := client.RetryJob(context.Background(),
				connect.NewRequest(&v1.RetryJobRequest{SessionId: first.Msg.SessionId}))
			Expect(err).NotTo(HaveOccurred())
			Eventually(stateOf(retried.Msg.SessionId)).Should(Equal("completed"),
				"the retry resolves the same definition rather than a name nothing defines")

			sess, err := sessionMgr.Get(context.Background(), retried.Msg.SessionId)
			Expect(err).NotTo(HaveOccurred())
			Expect(sess.ModeSpecJSON).To(ContainSubstring(`"extends":"review"`))
			Expect(mockForgeInst.commentCalls).To(Equal(2))
		})
	})

	Describe("a mode name the orchestrator cannot see yet", func() {
		It("is accepted at submission and resolved once the repository is cloned", func() {
			startService()
			resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project:   "test-project",
				Prompt:    "review this",
				Mode:      "review-pr",
				StartFrom: &v1.StartJobRequest_PrRef{PrRef: "42"},
			}))
			Expect(err).NotTo(HaveOccurred(), "submission cannot read the repository's modes")
			Eventually(stateOf(resp.Msg.SessionId)).Should(Equal("completed"))
		})

		It("fails the run, naming what the project does define, when nothing defines it", func() {
			startService()
			resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project: "test-project",
				Prompt:  "review this",
				Mode:    "nonesuch",
			}))
			Expect(err).NotTo(HaveOccurred())
			Eventually(stateOf(resp.Msg.SessionId)).Should(Equal("failed"))

			sess, err := sessionMgr.Get(context.Background(), resp.Msg.SessionId)
			Expect(err).NotTo(HaveOccurred())
			Expect(sess.Error).To(ContainSubstring(`unknown mode "nonesuch"`))
			Expect(sess.Error).To(ContainSubstring("review-pr"), "the message lists what is available")
		})

		It("fails the run when the repository's own modes do not parse", func() {
			kvarnYML = "modes:\n  bad:\n    workspace: read-mostly\n"
			startService()
			resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project: "test-project",
				Prompt:  "do it",
			}))
			Expect(err).NotTo(HaveOccurred())
			Eventually(stateOf(resp.Msg.SessionId)).Should(Equal("failed"))

			sess, err := sessionMgr.Get(context.Background(), resp.Msg.SessionId)
			Expect(err).NotTo(HaveOccurred())
			Expect(sess.Error).To(ContainSubstring("invalid workspace"))
		})

		It("fails the run before booting a VM when the mode has nowhere to deliver", func() {
			kvarnYML = "modes:\n  revise:\n    extends: feedback\n"
			startService()
			resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project: "test-project",
				Prompt:  "revise this",
				Mode:    "revise",
			}))
			Expect(err).NotTo(HaveOccurred(), "submission cannot read the repository's modes")
			Eventually(stateOf(resp.Msg.SessionId)).Should(Equal("failed"))

			sess, err := sessionMgr.Get(context.Background(), resp.Msg.SessionId)
			Expect(err).NotTo(HaveOccurred())
			Expect(sess.Error).To(ContainSubstring("existing pull request"))
			Expect(testAgent.lastPrompt()).To(BeEmpty(), "the agent never ran")
		})

		It("fails the run when the mode cannot start where the job did", func() {
			startService()
			resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project: "test-project",
				Prompt:  "review this",
				Mode:    "review-pr",
			}))
			Expect(err).NotTo(HaveOccurred(), "submission cannot know the mode needs a pull request")
			Eventually(stateOf(resp.Msg.SessionId)).Should(Equal("failed"))

			sess, err := sessionMgr.Get(context.Background(), resp.Msg.SessionId)
			Expect(err).NotTo(HaveOccurred())
			Expect(sess.Error).To(ContainSubstring("existing pull request"))
		})
	})
})
