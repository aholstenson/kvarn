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
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
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

// capturingAgent records the task message it was handed and edits a tracked
// file so there is a follow-up delta to submit.
type capturingAgent struct {
	mu     sync.Mutex
	prompt string
	// revise answers a request to revise the pull request text. Nil fails
	// the request, which a delivery treats as "keep the current text".
	revise func(agent.PullRequestRevision) (*agent.Result, error)
	// revisions records every revision request, in order.
	revisions []agent.PullRequestRevision
}

func (a *capturingAgent) revisionRequests() []agent.PullRequestRevision {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]agent.PullRequestRevision(nil), a.revisions...)
}

func (a *capturingAgent) lastPrompt() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.prompt
}

func (a *capturingAgent) Start(_ context.Context, agentCtx *agent.Context) (agent.Conversation, error) {
	a.mu.Lock()
	a.prompt = agentCtx.Prompt
	a.mu.Unlock()
	return &capturingConversation{agent: a, agentCtx: agentCtx}, nil
}

type capturingConversation struct {
	agent    *capturingAgent
	agentCtx *agent.Context
}

func (c *capturingConversation) RevisePullRequest(
	_ context.Context, in agent.PullRequestRevision,
) (*agent.Result, error) {
	c.agent.mu.Lock()
	c.agent.revisions = append(c.agent.revisions, in)
	revise := c.agent.revise
	c.agent.mu.Unlock()
	if revise == nil {
		return nil, fmt.Errorf("no revision scripted")
	}
	return revise(in)
}

func (c *capturingConversation) Run(_ context.Context, _ string) (string, error) {
	path := filepath.Join(c.agentCtx.WorkingDir, "README.md")
	return "", os.WriteFile(path, []byte("# Test\n\nrenamed helper\n"), 0644)
}

func (c *capturingConversation) Summarize(_ context.Context) (*agent.Result, error) {
	return &agent.Result{
		Title:       "Rename the helper",
		Description: "Renamed the helper and added a test for it.",
	}, nil
}

func (c *capturingConversation) Close() error { return nil }

var _ = Describe("StartJob continuing a pull request", func() {
	var (
		client        kvarnv1connect.OrchestratorServiceClient
		server        *http.Server
		mockScm       *mockSCM
		mockForgeInst *mockForge
		sessionMgr    session.Manager
		listener      net.Listener
		tmpDir        string
		testAgent     *capturingAgent
		projStore     *memProjectStore
		// gate, when non-nil, blocks the sandbox factory so a job stays
		// in-flight for as long as the test needs it to.
		gate chan struct{}
	)

	// openPR is the pull request the happy-path cases operate on.
	openPR := func() *forge.PullRequestDetails {
		return &forge.PullRequestDetails{
			Ref:        "42",
			State:      "open",
			HeadBranch: "kvarn/add-a-helper",
			HeadSHA:    "abc123",
			HeadRepo:   "owner/repo",
			BaseBranch: "master",
			BaseRepo:   "owner/repo",
			Title:      "Add a helper",
			Body:       "Adds a small helper.",
			URL:        "https://github.com/owner/repo/pull/42",
		}
	}

	// activeSessions counts sessions in any state, to assert that a rejected
	// request left no trace.
	activeSessions := func() int {
		all, err := sessionMgr.List(context.Background(), session.SessionFilter{})
		Expect(err).NotTo(HaveOccurred())
		return len(all)
	}

	BeforeEach(func() {
		mockScm = &mockSCM{}
		sessionMgr = session.NewManager(session.NewMemStore())
		testAgent = &capturingAgent{}
		gate = nil

		var err error
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())

		tmpDir, err = os.MkdirTemp("", "feedback-test-*")
		Expect(err).NotTo(HaveOccurred())

		projStore = &memProjectStore{
			projects: map[string]*project.Project{
				"test-project": {
					Name:          "test-project",
					RepoURL:       filepath.Join(tmpDir, "repo.git"),
					DefaultBranch: "master",
					Forge:         "test-forge",
				},
				"forgeless": {
					Name:          "forgeless",
					RepoURL:       filepath.Join(tmpDir, "repo.git"),
					DefaultBranch: "master",
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

		factory := func(ctx context.Context, opts sandbox.Opts) (orchestrator.Sandbox, error) {
			if gate != nil {
				select {
				case <-gate:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			wsDir, err := os.MkdirTemp("", "feedback-ws-*")
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
				Expect(os.WriteFile(filepath.Join(wsDir, "README.md"), []byte("# Test\n"), 0644)).To(Succeed())
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

		mockForgeInst = &mockForge{
			scmImpl:      mockScm,
			pullRequests: map[string]*forge.PullRequestDetails{"42": openPR()},
			diff:         "diff --git a/helper.go b/helper.go\n+func helper() {}\n",
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
			PreviewPolicy:    orchestrator.PreviewPolicy{Domain: "preview.example.com"},
		})

		mux := http.NewServeMux()
		orchestratorRPC := connect.NewServer()
		kvarnv1connect.RegisterOrchestratorServiceHandler(orchestratorRPC, svc)
		connecthttp.Mount(mux, orchestratorRPC)
		bridgeRPC := connect.NewServer()
		kvarnv1connect.RegisterBridgeServiceHandler(bridgeRPC, svc.BridgeHandler())
		connecthttp.Mount(mux, bridgeRPC)

		server = &http.Server{Handler: mux}
		go server.Serve(listener)

		client = kvarnv1connect.NewOrchestratorServiceClient(connect.NewClient(connecthttp.NewTransport(
			http.DefaultClient,
			fmt.Sprintf("http://%s", listener.Addr().String()),
			connecthttp.WithReadMaxBytes(0),
		)))
	})

	AfterEach(func() {
		if gate != nil {
			select {
			case <-gate:
			default:
				close(gate)
			}
		}
		server.Close()
		os.RemoveAll(tmpDir)
	})

	submit := func(project, prRef, feedback string) (*v1.StartJobResponse, error) {
		return client.StartJob(context.Background(), &v1.StartJobRequest{
			Project:   project,
			Prompt:    feedback,
			StartFrom: &v1.StartJobRequest_PrRef{PrRef: prRef},
		})
	}

	stateOf := func(sid string) func() string {
		return func() string {
			s, err := sessionMgr.Get(context.Background(), sid)
			if err != nil {
				return ""
			}
			return string(s.State)
		}
	}

	It("clones the head branch, pushes back to it, and comments without opening a PR", func() {
		resp, err := submit("test-project", "42", "rename the helper and add a test")
		Expect(err).NotTo(HaveOccurred())
		Eventually(stateOf(resp.SessionId)).Should(Equal("completed"))

		mockScm.mu.Lock()
		cloneBranch := mockScm.lastCloneOpts.Branch
		pushOpts := mockScm.lastPushOpts
		pushCalls := mockScm.pushCalls
		mockScm.mu.Unlock()

		Expect(cloneBranch).To(Equal("kvarn/add-a-helper"))
		Expect(pushCalls).To(Equal(1))
		Expect(pushOpts.Branch).To(Equal("kvarn/add-a-helper"))
		Expect(pushOpts.Message).To(HavePrefix("Rename the helper"))

		Expect(mockForgeInst.prCalls).To(Equal(0))
		Expect(mockForgeInst.commentCalls).To(Equal(1))
		Expect(mockForgeInst.lastCommentOpts.PRRef).To(Equal("42"))
		Expect(mockForgeInst.lastCommentOpts.Body).To(ContainSubstring("## Feedback addressed"))
		Expect(mockForgeInst.lastCommentOpts.Body).To(ContainSubstring("rename the helper and add a test"))
		Expect(mockForgeInst.lastCommentOpts.Body).To(ContainSubstring("Renamed the helper"))
	})

	It("links the branch's preview from the follow-up comment", func() {
		mockScm.files = map[string][]byte{
			"kvarn.yml": []byte("preview:\n  sites:\n    web:\n      port: 3000\n      host: \"pr-{pr}.{domain}\"\n"),
		}
		projStore.projects["test-project"].PullRequest = forgeconfig.PRContent{
			CommentHeaders: forgeconfig.CommentHeaders{
				FollowUpCommit: "{{ with .PreviewURL }}**Preview:** {{ . }}{{ end }}",
			},
		}

		resp, err := submit("test-project", "42", "rename the helper")
		Expect(err).NotTo(HaveOccurred())
		Eventually(stateOf(resp.SessionId)).Should(Equal("completed"))

		Expect(mockForgeInst.lastCommentOpts.Body).To(HavePrefix("**Preview:** https://pr-42.preview.example.com"),
			"the pull request is open, so the site addressed by {pr} resolves without anything having started it")
	})

	It("records the pull request on the session and keeps the raw feedback as its prompt", func() {
		resp, err := submit("test-project", "42", "rename the helper")
		Expect(err).NotTo(HaveOccurred())
		Eventually(stateOf(resp.SessionId)).Should(Equal("completed"))

		got, err := client.GetSession(context.Background(), &v1.GetSessionRequest{
			SessionId: resp.SessionId,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.PrRef).To(Equal("42"))
		Expect(got.HeadBranch).To(Equal("kvarn/add-a-helper"))
		Expect(got.BaseBranch).To(Equal("master"))
		Expect(got.PullRequestUrl).To(Equal("https://github.com/owner/repo/pull/42"))
		Expect(got.Mode).To(Equal("feedback"))
		Expect(got.Prompt).To(Equal("rename the helper"))
	})

	It("hands the agent a context pack with the original task, PR, diff, and feedback", func() {
		parent, err := sessionMgr.Create(context.Background(), session.CreateParams{
			ProjectName: "test-project",
			Prompt:      "add a trivial helper",
			Mode:        "auto",
			PRRef:       "42",
			HeadBranch:  "kvarn/add-a-helper",
			BaseBranch:  "master",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(sessionMgr.UpdateState(context.Background(), parent.ID, session.StateCompleted, "done")).To(Succeed())

		resp, err := submit("test-project", "42", "rename the helper")
		Expect(err).NotTo(HaveOccurred())
		Eventually(stateOf(resp.SessionId)).Should(Equal("completed"))

		prompt := testAgent.lastPrompt()
		Expect(prompt).To(ContainSubstring("## Original task"))
		Expect(prompt).To(ContainSubstring("add a trivial helper"))
		Expect(prompt).To(ContainSubstring("## Current pull request"))
		Expect(prompt).To(ContainSubstring("Add a helper"))
		Expect(prompt).To(ContainSubstring("## Diff"))
		Expect(prompt).To(ContainSubstring("func helper()"))
		Expect(prompt).To(ContainSubstring("## Feedback to address"))
		Expect(prompt).To(ContainSubstring("rename the helper"))

		got, err := sessionMgr.Get(context.Background(), resp.SessionId)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.ParentSessionID).To(Equal(parent.ID))
	})

	It("omits the original task when no prior session survives", func() {
		resp, err := submit("test-project", "42", "rename the helper")
		Expect(err).NotTo(HaveOccurred())
		Eventually(stateOf(resp.SessionId)).Should(Equal("completed"))

		Expect(testAgent.lastPrompt()).NotTo(ContainSubstring("## Original task"))

		got, err := sessionMgr.Get(context.Background(), resp.SessionId)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.ParentSessionID).To(BeEmpty())
	})

	It("fails the run rather than pushing when the head moved underneath it", func() {
		// The pre-push re-read reports a different tip than the one the run
		// started from: the head moves after the submission check and the
		// dispatcher's context pack have both read it.
		mockForgeInst.movedHeadSHA = "def456"
		mockForgeInst.moveHeadAfterCall = 2

		resp, err := submit("test-project", "42", "rename the helper")
		Expect(err).NotTo(HaveOccurred())

		Eventually(stateOf(resp.SessionId)).Should(Equal("failed"))
		got, err := sessionMgr.Get(context.Background(), resp.SessionId)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Error).To(ContainSubstring("moved during the run"))

		mockScm.mu.Lock()
		defer mockScm.mu.Unlock()
		Expect(mockScm.pushCalls).To(Equal(0))
	})

	It("rejects a fork pull request without creating a session", func() {
		fork := openPR()
		fork.HeadRepo = "contributor/repo"
		mockForgeInst.pullRequests["42"] = fork

		_, err := submit("test-project", "42", "rename the helper")
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(err.Error()).To(ContainSubstring("fork"))
		Expect(activeSessions()).To(Equal(0))
	})

	It("rejects a pull request that is not open without creating a session", func() {
		merged := openPR()
		merged.State = "merged"
		mockForgeInst.pullRequests["42"] = merged

		_, err := submit("test-project", "42", "rename the helper")
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("merged"))
		Expect(activeSessions()).To(Equal(0))
	})

	It("rejects an unknown pull request ref without creating a session", func() {
		_, err := submit("test-project", "999", "rename the helper")
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(activeSessions()).To(Equal(0))
	})

	It("rejects a project with no forge configured without creating a session", func() {
		_, err := submit("forgeless", "42", "rename the helper")
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("no forge configured"))
		Expect(activeSessions()).To(Equal(0))
	})

	It("rejects a second run while one is in flight for the same pull request", func() {
		gate = make(chan struct{})

		first, err := submit("test-project", "42", "rename the helper")
		Expect(err).NotTo(HaveOccurred())

		_, err = submit("test-project", "42", "also add a test")
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring(first.SessionId))
		Expect(activeSessions()).To(Equal(1))

		// Once the first run finishes, the pull request accepts work again.
		close(gate)
		Eventually(stateOf(first.SessionId)).Should(Equal("completed"))

		_, err = submit("test-project", "42", "also add a test")
		Expect(err).NotTo(HaveOccurred())
	})

	It("retries a continuation against the same pull request rather than as a fresh job", func() {
		first, err := submit("test-project", "42", "rename the helper")
		Expect(err).NotTo(HaveOccurred())
		Eventually(stateOf(first.SessionId)).Should(Equal("completed"))

		retried, err := client.RetryJob(context.Background(), &v1.RetryJobRequest{
			SessionId: first.SessionId,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(retried.SessionId).NotTo(Equal(first.SessionId))

		// The retry is a continuation of the same pull request. Were the marker
		// not persisted, the session's PR ref would read as one this job opened
		// and the retry would be refused outright.
		sess, err := sessionMgr.Get(context.Background(), retried.SessionId)
		Expect(err).NotTo(HaveOccurred())
		Expect(sess.Continuation).To(BeTrue())
		Expect(sess.PRRef).To(Equal("42"))
	})

	Describe("revising the pull request text", func() {
		const footer = "Run {{ .SessionID }}"

		// opened is the text kvarn wrote when it opened pull request 42. It
		// matches openPR(), so the forge still holds exactly what kvarn wrote.
		opened := session.PullRequestText{
			Title:       "Add a helper",
			Body:        "Adds a small helper.",
			Description: "Adds a small helper.",
		}

		// seedOpener records a finished session that opened pull request 42
		// and wrote text to it.
		seedOpener := func(text session.PullRequestText) {
			ctx := context.Background()
			parent, err := sessionMgr.Create(ctx, session.CreateParams{
				ProjectName: "test-project",
				Prompt:      "add a trivial helper",
				Mode:        "auto",
				PRRef:       "42",
				HeadBranch:  "kvarn/add-a-helper",
				BaseBranch:  "master",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(sessionMgr.UpdateState(ctx, parent.ID, session.StateCompleted, "done")).To(Succeed())
			Expect(sessionMgr.SetPullRequestWritten(ctx, parent.ID, text)).To(Succeed())
		}

		storedPR := func() forge.PullRequestDetails {
			mockForgeInst.mu.Lock()
			defer mockForgeInst.mu.Unlock()
			return *mockForgeInst.pullRequests["42"]
		}

		runFollowUp := func() *session.Session {
			resp, err := submit("test-project", "42", "rename the helper")
			Expect(err).NotTo(HaveOccurred())
			Eventually(stateOf(resp.SessionId)).WithTimeout(10 * time.Second).Should(Equal("completed"))
			got, err := sessionMgr.Get(context.Background(), resp.SessionId)
			Expect(err).NotTo(HaveOccurred())
			return got
		}

		BeforeEach(func() {
			projStore.projects["test-project"].PullRequest = forgeconfig.PRContent{BodyFooter: footer}
			testAgent.revise = func(agent.PullRequestRevision) (*agent.Result, error) {
				return &agent.Result{
					Title:       "Add a renamed helper",
					Description: "Adds a small renamed helper with a test.",
				}, nil
			}
		})

		It("rewrites the title and description of a pull request kvarn opened", func() {
			seedOpener(opened)
			got := runFollowUp()

			requests := testAgent.revisionRequests()
			Expect(requests).To(HaveLen(1))
			Expect(requests[0].Title).To(Equal("Add a helper"))
			Expect(requests[0].Description).To(Equal("Adds a small helper."))
			Expect(requests[0].Diff).To(ContainSubstring("func helper()"))

			pr := storedPR()
			Expect(pr.Title).To(Equal("Add a renamed helper"))
			Expect(pr.Body).To(Equal("Adds a small renamed helper with a test.\n\nRun " + got.ID))

			Expect(got.PullRequestWritten).To(Equal(&session.PullRequestText{
				Title:       "Add a renamed helper",
				Body:        pr.Body,
				Description: "Adds a small renamed helper with a test.",
			}), "the newest session takes the record, so the next follow-up compares against this text")
			Expect(mockForgeInst.lastCommentOpts.Body).To(
				ContainSubstring("Updated the pull request title and description to match the change as it is now."))
		})

		It("leaves a pull request alone when no session on record wrote its text", func() {
			got := runFollowUp()

			Expect(testAgent.revisionRequests()).To(BeEmpty())
			Expect(mockForgeInst.updateCalls).To(BeZero())
			Expect(got.PullRequestWritten).To(BeNil())
			Expect(mockForgeInst.lastCommentOpts.Body).NotTo(ContainSubstring("pull request title"))
		})

		It("keeps a description a person edited and still revises the title", func() {
			seedOpener(opened)
			mockForgeInst.mu.Lock()
			mockForgeInst.pullRequests["42"].Body = "Adds a small helper.\n\nEdited by a reviewer."
			mockForgeInst.mu.Unlock()

			got := runFollowUp()

			Expect(testAgent.revisionRequests()[0].Description).To(
				Equal("Adds a small helper.\n\nEdited by a reviewer."),
				"the person's body is context for the title")
			Expect(mockForgeInst.lastUpdateOpts.Body).To(BeNil())
			pr := storedPR()
			Expect(pr.Title).To(Equal("Add a renamed helper"))
			Expect(pr.Body).To(Equal("Adds a small helper.\n\nEdited by a reviewer."))

			Expect(got.PullRequestWritten.Body).To(Equal(opened.Body),
				"the body keeps kvarn's last write, which no longer matches, so it stays the person's")
			Expect(mockForgeInst.lastCommentOpts.Body).To(ContainSubstring(
				"Updated the pull request title to match the change as it is now. " +
					"The description was edited by hand, so this run left it as it is."))
		})

		It("does not ask for a revision when a person edited both fields", func() {
			seedOpener(opened)
			mockForgeInst.mu.Lock()
			mockForgeInst.pullRequests["42"].Title = "A title a person chose"
			mockForgeInst.pullRequests["42"].Body = "A body a person wrote"
			mockForgeInst.mu.Unlock()

			got := runFollowUp()

			Expect(testAgent.revisionRequests()).To(BeEmpty())
			Expect(mockForgeInst.updateCalls).To(BeZero())
			Expect(got.PullRequestWritten).To(Equal(&opened))
			Expect(mockForgeInst.lastCommentOpts.Body).To(ContainSubstring(
				"The title and description were edited by hand, so this run left them as they are."))
		})

		It("writes nothing when a person edits the text while the revision runs", func() {
			seedOpener(opened)
			testAgent.revise = func(agent.PullRequestRevision) (*agent.Result, error) {
				mockForgeInst.mu.Lock()
				mockForgeInst.pullRequests["42"].Title = "Retitled mid-run"
				mockForgeInst.pullRequests["42"].Body = "Rewritten mid-run"
				mockForgeInst.mu.Unlock()
				return &agent.Result{Title: "Add a renamed helper", Description: "Revised."}, nil
			}

			runFollowUp()

			Expect(mockForgeInst.updateCalls).To(BeZero())
			pr := storedPR()
			Expect(pr.Title).To(Equal("Retitled mid-run"))
			Expect(pr.Body).To(Equal("Rewritten mid-run"))
		})

		It("leaves the body alone when the revised description is unchanged", func() {
			seedOpener(opened)
			testAgent.revise = func(in agent.PullRequestRevision) (*agent.Result, error) {
				return &agent.Result{Title: in.Title, Description: in.Description}, nil
			}

			got := runFollowUp()

			Expect(mockForgeInst.updateCalls).To(BeZero(),
				"only the footer would differ, and that is not worth a change to the pull request")
			Expect(got.PullRequestWritten).To(Equal(&opened))
			Expect(mockForgeInst.lastCommentOpts.Body).NotTo(ContainSubstring("pull request title"))
		})

		It("matches text whose line endings the forge changed", func() {
			seedOpener(session.PullRequestText{
				Title:       "Add a helper",
				Body:        "Adds a small helper.\nIn two lines.",
				Description: "Adds a small helper.\nIn two lines.",
			})
			mockForgeInst.mu.Lock()
			mockForgeInst.pullRequests["42"].Body = "Adds a small helper.\r\nIn two lines.\r\n"
			mockForgeInst.mu.Unlock()

			runFollowUp()

			Expect(mockForgeInst.lastUpdateOpts.Body).NotTo(BeNil())
		})

		It("keeps the current text when the revision fails", func() {
			seedOpener(opened)
			testAgent.revise = nil

			got := runFollowUp()

			Expect(mockForgeInst.updateCalls).To(BeZero())
			Expect(got.PullRequestWritten).To(Equal(&opened))
		})

		It("keeps the run completed when the forge refuses the update", func() {
			seedOpener(opened)
			mockForgeInst.updateErr = fmt.Errorf("HTTP 403")

			got := runFollowUp()

			Expect(mockForgeInst.updateCalls).To(Equal(1))
			Expect(got.PullRequestWritten).To(Equal(&opened),
				"the forge kept the old text, so the record must too")
		})

		It("leaves the text alone when the operator turned the update off", func() {
			off := false
			projStore.projects["test-project"].PullRequest.UpdateOnFollowUp = &off
			seedOpener(opened)

			runFollowUp()

			Expect(testAgent.revisionRequests()).To(BeEmpty())
			Expect(mockForgeInst.updateCalls).To(BeZero())
		})
	})

	Describe("idempotency", func() {
		submitWithKey := func(prRef, feedback, key string) (*v1.StartJobResponse, error) {
			return client.StartJob(context.Background(), &v1.StartJobRequest{
				Project:        "test-project",
				Prompt:         feedback,
				StartFrom:      &v1.StartJobRequest_PrRef{PrRef: prRef},
				IdempotencyKey: key,
			})
		}

		It("returns the first session when the same key is replayed after the run finished", func() {
			first, err := submitWithKey("42", "rename the helper", "req-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(first.Duplicate).To(BeFalse())
			Eventually(stateOf(first.SessionId)).Should(Equal("completed"))

			second, err := submitWithKey("42", "rename the helper", "req-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(second.SessionId).To(Equal(first.SessionId))
			Expect(second.Duplicate).To(BeTrue())

			// The point of the key: one run, so one follow-up commit and one
			// comment on the pull request.
			Expect(activeSessions()).To(Equal(1))
			mockScm.mu.Lock()
			pushCalls := mockScm.pushCalls
			mockScm.mu.Unlock()
			Expect(pushCalls).To(Equal(1))
			Expect(mockForgeInst.commentCalls).To(Equal(1))
		})

		It("returns the in-flight session rather than refusing the retry as a second run", func() {
			gate = make(chan struct{})

			first, err := submitWithKey("42", "rename the helper", "req-1")
			Expect(err).NotTo(HaveOccurred())

			// Without the key this is the "already running" rejection above.
			// With it, the retry is asking about the very run that is holding
			// the pull request, so it gets that run back.
			second, err := submitWithKey("42", "rename the helper", "req-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(second.SessionId).To(Equal(first.SessionId))
			Expect(second.Duplicate).To(BeTrue())
			Expect(activeSessions()).To(Equal(1))

			close(gate)
			Eventually(stateOf(first.SessionId)).Should(Equal("completed"))
		})

		It("matches a replay that spells the pull request the way the forge does not", func() {
			// The forge resolves both spellings to the same pull request, so the
			// replay is the same submission even though the request strings
			// differ. Matching happens against the ref the forge returned.
			alt := openPR()
			mockForgeInst.pullRequests["#42"] = alt

			first, err := submitWithKey("42", "rename the helper", "req-1")
			Expect(err).NotTo(HaveOccurred())
			Eventually(stateOf(first.SessionId)).Should(Equal("completed"))

			second, err := submitWithKey("#42", "rename the helper", "req-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(second.SessionId).To(Equal(first.SessionId))
			Expect(second.Duplicate).To(BeTrue())
			Expect(activeSessions()).To(Equal(1))
		})

		It("starts a second run for a different key", func() {
			first, err := submitWithKey("42", "rename the helper", "req-1")
			Expect(err).NotTo(HaveOccurred())
			Eventually(stateOf(first.SessionId)).Should(Equal("completed"))

			second, err := submitWithKey("42", "also add a test", "req-2")
			Expect(err).NotTo(HaveOccurred())
			Expect(second.SessionId).NotTo(Equal(first.SessionId))
			Expect(second.Duplicate).To(BeFalse())
		})

		It("refuses a key reused for different feedback", func() {
			first, err := submitWithKey("42", "rename the helper", "req-1")
			Expect(err).NotTo(HaveOccurred())
			Eventually(stateOf(first.SessionId)).Should(Equal("completed"))

			_, err = submitWithKey("42", "something else entirely", "req-1")
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeAlreadyExists))
			Expect(activeSessions()).To(Equal(1))
		})

		It("rejects an oversized key", func() {
			_, err := submitWithKey("42", "rename the helper", strings.Repeat("k", 256))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(activeSessions()).To(Equal(0))
		})
	})
})
