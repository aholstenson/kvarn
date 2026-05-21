package orchestrator_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/config/credential"
	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/config/secret"
	"github.com/aholstenson/kvarn/internal/forge"
	"github.com/aholstenson/kvarn/internal/orchestrator"
	projconfig "github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/runner"
	"github.com/aholstenson/kvarn/internal/sandbox"
	"github.com/aholstenson/kvarn/internal/scm"
	"github.com/aholstenson/kvarn/internal/session"
	"github.com/aholstenson/kvarn/internal/vm"
	stderrors "errors"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// memCredentialStore is an in-memory credential.Store for tests.
type memCredentialStore struct {
	creds map[string]*credential.Credential
}

func (s *memCredentialStore) Get(_ context.Context, name string) (*credential.Credential, error) {
	c, ok := s.creds[name]
	if !ok {
		return nil, fmt.Errorf("credential %q not found", name)
	}
	return c, nil
}

func (s *memCredentialStore) List(_ context.Context) ([]*credential.Credential, error) {
	var out []*credential.Credential
	for _, c := range s.creds {
		out = append(out, c)
	}
	return out, nil
}

func (s *memCredentialStore) Put(_ context.Context, c *credential.Credential) error {
	s.creds[c.Name] = c
	return nil
}

func (s *memCredentialStore) Delete(_ context.Context, name string) error {
	delete(s.creds, name)
	return nil
}

// scriptedAgent emits a fixed sequence of progress events, writes a file to
// the working directory so there are changes to submit, and returns a fixed
// summary.
type scriptedAgent struct {
	title       string
	description string
	fileName    string
	fileBody    string
}

func (a *scriptedAgent) Run(_ context.Context, agentCtx *agent.Context) (*agent.Result, error) {
	if a.fileName != "" {
		path := filepath.Join(agentCtx.WorkingDir, a.fileName)
		if err := os.WriteFile(path, []byte(a.fileBody), 0644); err != nil {
			return nil, err
		}
	}
	if agentCtx.OnProgress != nil {
		agentCtx.OnProgress(agent.ProgressTextMessage{Text: "Planning the change."})
		agentCtx.OnProgress(agent.ProgressToolUse{ToolID: "WriteFile", ArgumentsJSON: `{"path":"` + a.fileName + `"}`})
		agentCtx.OnProgress(agent.ProgressToolResult{ToolID: "WriteFile", Result: "ok"})
		agentCtx.OnProgress(agent.ProgressToolUse{ToolID: "Bash", ArgumentsJSON: `{"cmd":"go test"}`})
		agentCtx.OnProgress(agent.ProgressToolResult{ToolID: "Bash", Result: "test failure: thing broke", IsError: true})
	}
	return &agent.Result{
		Title:       a.title,
		Description: a.description,
	}, nil
}

// localRunnerProxy implements sandbox.RunnerProxy by delegating to a local
// runner.Handler, avoiding the need for a real VM or bridge connection.
type localRunnerProxy struct {
	handler *runner.Handler
}

func (p *localRunnerProxy) Exec(ctx context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
	resp, err := p.handler.Exec(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (p *localRunnerProxy) CreateSession(ctx context.Context, req *v1.CreateSessionRequest) (*v1.CreateSessionResponse, error) {
	resp, err := p.handler.CreateSession(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (p *localRunnerProxy) SessionExec(ctx context.Context, req *v1.SessionExecRequest, onOutput sandbox.OutputCallback) (*v1.SessionExecResponse, error) {
	var runnerCb runner.OutputCallback
	if onOutput != nil {
		runnerCb = runner.OutputCallback(onOutput)
	}
	resp, err := p.handler.SessionExecWithOutput(ctx, req, runnerCb)
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (p *localRunnerProxy) CloseSession(ctx context.Context, req *v1.CloseSessionRequest) (*v1.CloseSessionResponse, error) {
	resp, err := p.handler.CloseSession(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (p *localRunnerProxy) UploadFiles(ctx context.Context, req *v1.UploadFilesRequest) (*v1.UploadFilesResponse, error) {
	resp, err := p.handler.UploadFiles(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (p *localRunnerProxy) ReadFile(ctx context.Context, req *v1.ReadFileRequest) (*v1.ReadFileResponse, error) {
	resp, err := p.handler.ReadFile(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (p *localRunnerProxy) EditFile(ctx context.Context, req *v1.EditFileRequest) (*v1.EditFileResponse, error) {
	resp, err := p.handler.EditFile(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (p *localRunnerProxy) WriteFile(ctx context.Context, req *v1.WriteFileRequest) (*v1.WriteFileResponse, error) {
	resp, err := p.handler.WriteFile(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (p *localRunnerProxy) StreamToGuest(_ context.Context, _ string, _ io.Reader, _ int64) error {
	return stderrors.ErrUnsupported
}

func (p *localRunnerProxy) StreamFromGuest(_ context.Context, srcPath string, dest io.Writer) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(dest, f)
	return err
}

// testSandbox implements orchestrator.Sandbox without a real VM.
type testSandbox struct {
	runner         sandbox.RunnerProxy
	shellSessionID string
	workingDir     string
}

func (m *testSandbox) GetRunner() sandbox.RunnerProxy   { return m.runner }
func (m *testSandbox) GetShellSessionID() string         { return m.shellSessionID }
func (m *testSandbox) GetWorkingDir() string             { return m.workingDir }
func (m *testSandbox) SaveCache(_ context.Context) error { return nil }
func (m *testSandbox) Close()                            {}

func (m *testSandbox) RunSetup(ctx context.Context, cfg *projconfig.Config, onDone sandbox.OnStepDone, onOutput sandbox.OnOutput) (*sandbox.SetupResult, error) {
	if cfg == nil {
		return &sandbox.SetupResult{}, nil
	}
	return sandbox.RunSetup(ctx, m.runner, cfg, m.shellSessionID, onDone, onOutput)
}

func (m *testSandbox) RunValidation(ctx context.Context, cfg *projconfig.Config, changedFiles []string, onDone sandbox.OnStepDone, onOutput sandbox.OnOutput) (*sandbox.ValidationResult, error) {
	if cfg == nil {
		return &sandbox.ValidationResult{RequiredPassed: true}, nil
	}
	return sandbox.RunValidation(ctx, m.runner, cfg, m.shellSessionID, changedFiles, onDone, onOutput)
}

func (m *testSandbox) ChangedFiles(ctx context.Context) ([]string, error) {
	return sandbox.ChangedFiles(ctx, m.runner, m.workingDir)
}

func (m *testSandbox) ExtractChanges(ctx context.Context, destDir string) error {
	return sandbox.ExtractChanges(ctx, m.runner, m.workingDir, destDir)
}

// mockProvider simulates a VM by spawning a goroutine that acts as a runner:
// connects to the orchestrator's BridgeService, receives commands, executes
// them locally via runner.Handler, and reports results.
type mockProvider struct {
	orchestratorAddr string // set by test setup; runner connects here
	destroyed        atomic.Int32
	createErr        error
	destroyErr       error
}

func (m *mockProvider) Name() string { return "mock" }

func (m *mockProvider) PrepareImage(_ context.Context, base vm.BaseImage) (*vm.ProviderImage, error) {
	return &vm.ProviderImage{Base: &base}, nil
}

func (m *mockProvider) Create(_ context.Context, opts vm.CreateOpts) (*vm.VM, *vm.RunnerConn, error) {
	if m.createErr != nil {
		return nil, nil, m.createErr
	}

	token := opts.Token

	instance := &vm.VM{
		ID:    fmt.Sprintf("mock-%d", m.destroyed.Load()+1),
		Token: token,
	}

	// Spawn a goroutine that acts as the runner: connect to orchestrator bridge.
	go func() {
		h := runner.NewHandler()
		client := kvarnv1connect.NewBridgeServiceClient(http.DefaultClient, fmt.Sprintf("http://%s", m.orchestratorAddr))

		stream, err := client.Register(context.Background(), connect.NewRequest(&v1.RegisterRequest{
			Token: token,
		}))
		if err != nil {
			return
		}
		defer stream.Close()

		for stream.Receive() {
			cmd := stream.Msg()
			result := &v1.CommandResult{
				CommandId: cmd.CommandId,
				Token:     token,
			}

			switch c := cmd.Command.(type) {
			case *v1.RunnerCommand_Exec:
				resp, execErr := h.Exec(context.Background(), connect.NewRequest(c.Exec))
				if execErr != nil {
					result.Error = execErr.Error()
				} else {
					result.Result = &v1.CommandResult_Exec{Exec: resp.Msg}
				}
			case *v1.RunnerCommand_UploadFiles:
				resp, execErr := h.UploadFiles(context.Background(), connect.NewRequest(c.UploadFiles))
				if execErr != nil {
					result.Error = execErr.Error()
				} else {
					result.Result = &v1.CommandResult_UploadFiles{UploadFiles: resp.Msg}
				}
			case *v1.RunnerCommand_ReadFile:
				resp, execErr := h.ReadFile(context.Background(), connect.NewRequest(c.ReadFile))
				if execErr != nil {
					result.Error = execErr.Error()
				} else {
					result.Result = &v1.CommandResult_ReadFile{ReadFile: resp.Msg}
				}
			case *v1.RunnerCommand_EditFile:
				resp, execErr := h.EditFile(context.Background(), connect.NewRequest(c.EditFile))
				if execErr != nil {
					result.Error = execErr.Error()
				} else {
					result.Result = &v1.CommandResult_EditFile{EditFile: resp.Msg}
				}
			case *v1.RunnerCommand_WriteFile:
				resp, execErr := h.WriteFile(context.Background(), connect.NewRequest(c.WriteFile))
				if execErr != nil {
					result.Error = execErr.Error()
				} else {
					result.Result = &v1.CommandResult_WriteFile{WriteFile: resp.Msg}
				}
			case *v1.RunnerCommand_CreateSession:
				resp, execErr := h.CreateSession(context.Background(), connect.NewRequest(c.CreateSession))
				if execErr != nil {
					result.Error = execErr.Error()
				} else {
					result.Result = &v1.CommandResult_CreateSession{CreateSession: resp.Msg}
				}
			case *v1.RunnerCommand_SessionExec:
				resp, execErr := h.SessionExec(context.Background(), connect.NewRequest(c.SessionExec))
				if execErr != nil {
					result.Error = execErr.Error()
				} else {
					result.Result = &v1.CommandResult_SessionExec{SessionExec: resp.Msg}
				}
			case *v1.RunnerCommand_CloseSession:
				resp, execErr := h.CloseSession(context.Background(), connect.NewRequest(c.CloseSession))
				if execErr != nil {
					result.Error = execErr.Error()
				} else {
					result.Result = &v1.CommandResult_CloseSession{CloseSession: resp.Msg}
				}
			case *v1.RunnerCommand_DownloadFile:
				// No-op for mock: just report success.
				result.Result = &v1.CommandResult_DownloadFileResult{DownloadFileResult: &v1.FileStreamResult{}}
			case *v1.RunnerCommand_UploadFile:
				// No-op for mock: just report success.
				result.Result = &v1.CommandResult_UploadFileResult{UploadFileResult: &v1.FileStreamResult{}}
			default:
				result.Error = "unknown command type"
			}

			if _, err := client.ReportResult(context.Background(), connect.NewRequest(result)); err != nil {
				return
			}
		}
	}()

	return instance, nil, nil
}

func (m *mockProvider) Destroy(_ context.Context, _ string) error {
	m.destroyed.Add(1)
	if m.destroyErr != nil {
		return m.destroyErr
	}
	return nil
}

func (m *mockProvider) List(_ context.Context) ([]*vm.VM, error) {
	return nil, nil
}

// mockSCM records clone calls and performs a real local clone.
// Use the files field to inject additional files into the "cloned" directory.
type mockSCM struct {
	cloneCalls int
	cloneErr   error
	files      map[string][]byte
}

func (m *mockSCM) Clone(_ context.Context, opts scm.CloneOpts) error {
	m.cloneCalls++
	if m.cloneErr != nil {
		return m.cloneErr
	}
	// Just create a file in the destination to simulate a clone.
	if err := os.MkdirAll(opts.Destination, 0755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(opts.Destination, "README.md"), []byte("# Test\n"), 0644); err != nil {
		return err
	}
	// Write any additional injected files.
	for name, content := range m.files {
		path := filepath.Join(opts.Destination, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(path, content, 0644); err != nil {
			return err
		}
	}
	return nil
}

func (m *mockSCM) CommitAndPush(_ context.Context, _ scm.CommitAndPushOpts) error {
	return nil
}

// mockForge wraps a mockSCM and records PR creation and comment calls.
type mockForge struct {
	scmImpl         scm.SCM
	prCalls         int
	prErr           error
	lastPROpts      forge.CreatePROpts
	commentCalls    int
	commentErr      error
	lastCommentOpts forge.PostCommentOpts
}

func (m *mockForge) SCM() scm.SCM { return m.scmImpl }

func (m *mockForge) ResolveCloneURL(repo string) (string, error) {
	return repo, nil
}

func (m *mockForge) ResolveCredentials(_ context.Context, config map[string]string) (*scm.Credentials, error) {
	creds := &scm.Credentials{}
	if token := config["token"]; token != "" {
		creds.Token = token
	}
	return creds, nil
}

func (m *mockForge) CreatePullRequest(_ context.Context, opts forge.CreatePROpts) (*forge.PullRequest, error) {
	m.prCalls++
	m.lastPROpts = opts
	if m.prErr != nil {
		return nil, m.prErr
	}
	return &forge.PullRequest{
		URL:    "https://github.com/test/repo/pull/1",
		Number: 1,
	}, nil
}

func (m *mockForge) PostComment(_ context.Context, opts forge.PostCommentOpts) error {
	m.commentCalls++
	m.lastCommentOpts = opts
	return m.commentErr
}

// memForgeConfigStore is an in-memory forgeconfig.Store for tests.
type memForgeConfigStore struct {
	configs map[string]*forgeconfig.ForgeConfig
}

func (s *memForgeConfigStore) Get(_ context.Context, name string) (*forgeconfig.ForgeConfig, error) {
	fc, ok := s.configs[name]
	if !ok {
		return nil, fmt.Errorf("forge config %q not found", name)
	}
	return fc, nil
}

func (s *memForgeConfigStore) List(_ context.Context) ([]*forgeconfig.ForgeConfig, error) {
	var result []*forgeconfig.ForgeConfig
	for _, fc := range s.configs {
		result = append(result, fc)
	}
	return result, nil
}

func (s *memForgeConfigStore) Put(_ context.Context, fc *forgeconfig.ForgeConfig) error {
	s.configs[fc.Name] = fc
	return nil
}

func (s *memForgeConfigStore) Delete(_ context.Context, name string) error {
	delete(s.configs, name)
	return nil
}

var _ = Describe("Orchestrator", func() {
	var (
		client   kvarnv1connect.OrchestratorServiceClient
		server   *http.Server
		mock     *mockProvider
		listener net.Listener
	)

	BeforeEach(func() {
		mock = &mockProvider{}

		var err error
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())

		mock.orchestratorAddr = listener.Addr().String()

		svc := orchestrator.NewService(mock, vm.CreateOpts{})

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
	})

	It("executes a job end-to-end", func() {
		resp, err := client.ExecuteJob(context.Background(), connect.NewRequest(&v1.ExecuteJobRequest{
			Command: "echo",
			Args:    []string{"hello from orchestrator"},
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.ExitCode).To(Equal(int32(0)))
		Expect(resp.Msg.Stdout).To(Equal("hello from orchestrator\n"))
		Expect(resp.Msg.VmId).NotTo(BeEmpty())
	})

	It("returns non-zero exit code from failed command", func() {
		resp, err := client.ExecuteJob(context.Background(), connect.NewRequest(&v1.ExecuteJobRequest{
			Command: "sh",
			Args:    []string{"-c", "exit 7"},
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.ExitCode).To(Equal(int32(7)))
	})

	It("returns error when provider fails to create VM", func() {
		mock.createErr = fmt.Errorf("out of capacity")
		_, err := client.ExecuteJob(context.Background(), connect.NewRequest(&v1.ExecuteJobRequest{
			Command: "echo",
			Args:    []string{"hello"},
		}))
		Expect(err).To(HaveOccurred())
	})

	It("destroys the VM after execution", func() {
		_, err := client.ExecuteJob(context.Background(), connect.NewRequest(&v1.ExecuteJobRequest{
			Command: "echo",
			Args:    []string{"hello"},
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(mock.destroyed.Load()).To(Equal(int32(1)))
	})

	It("destroys the VM even when the command fails", func() {
		resp, err := client.ExecuteJob(context.Background(), connect.NewRequest(&v1.ExecuteJobRequest{
			Command: "nonexistent-command-xyz",
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.ExitCode).NotTo(Equal(int32(0)))
		Expect(mock.destroyed.Load()).To(Equal(int32(1)))
	})
})

var _ = Describe("StartJob", func() {
	var (
		client        kvarnv1connect.OrchestratorServiceClient
		server        *http.Server
		mockScm       *mockSCM
		mockForgeInst *mockForge
		sessionMgr    *session.MemoryManager
		listener      net.Listener
		bareDir       string
		tmpDir        string
	)

	BeforeEach(func() {
		mockScm = &mockSCM{}
		sessionMgr = session.NewMemoryManager()

		var err error
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())

		// Create a bare git repo for the mock project.
		tmpDir, err = os.MkdirTemp("", "startjob-test-*")
		Expect(err).NotTo(HaveOccurred())

		bareDir = filepath.Join(tmpDir, "repo.git")
		cmd := exec.Command("git", "init", "--bare", bareDir)
		Expect(cmd.Run()).To(Succeed())

		workDir := filepath.Join(tmpDir, "work")
		cmd = exec.Command("git", "clone", bareDir, workDir)
		Expect(cmd.Run()).To(Succeed())

		Expect(os.WriteFile(filepath.Join(workDir, "hello.txt"), []byte("hello\n"), 0644)).To(Succeed())

		cmd = exec.Command("git", "add", "hello.txt")
		cmd.Dir = workDir
		Expect(cmd.Run()).To(Succeed())

		cmd = exec.Command("git", "-c", "user.email=test@test.com", "-c", "user.name=Test", "commit", "-m", "initial")
		cmd.Dir = workDir
		Expect(cmd.Run()).To(Succeed())

		cmd = exec.Command("git", "push", "origin", "HEAD")
		cmd.Dir = workDir
		Expect(cmd.Run()).To(Succeed())

		// Set up in-memory project store with one project.
		projStore := &memProjectStore{
			projects: map[string]*project.Project{
				"test-project": {
					Name:          "test-project",
					RepoURL:       bareDir,
					DefaultBranch: "master",
					Forge:         "test-forge",
				},
			},
		}

		// Set up in-memory forge config store.
		forgeConfigStore := &memForgeConfigStore{
			configs: map[string]*forgeconfig.ForgeConfig{
				"test-forge": {
					Name:         "test-forge",
					Type:         "mock",
					BranchPrefix: "kvarn",
				},
			},
		}

		// SandboxFactory copies cloned files into a local workspace dir,
		// creates a runner.Handler, and returns a testSandbox — no VM needed.
		factory := func(_ context.Context, opts sandbox.Opts) (orchestrator.Sandbox, error) {
			// Use a temp directory as the workspace — the configured
			// WorkingDir is a VM path that doesn't exist locally.
			wsDir, err := os.MkdirTemp("", "test-workspace-*")
			if err != nil {
				return nil, err
			}

			// Copy source files into workspace.
			if opts.SourceDir != "" {
				cpCmd := exec.Command("cp", "-a", opts.SourceDir+"/.", wsDir)
				if out, err := cpCmd.CombinedOutput(); err != nil {
					return nil, fmt.Errorf("copy source: %s: %w", out, err)
				}
			}

			// Initialize git repo in workspace so ChangedFiles works.
			if _, err := os.Stat(filepath.Join(wsDir, ".git")); os.IsNotExist(err) {
				Expect(os.MkdirAll(wsDir, 0755)).To(Succeed())
				initCmd := exec.Command("git", "init", wsDir)
				Expect(initCmd.Run()).To(Succeed())
				addCmd := exec.Command("git", "add", ".")
				addCmd.Dir = wsDir
				Expect(addCmd.Run()).To(Succeed())
				commitCmd := exec.Command("git", "-c", "user.email=test@test.com", "-c", "user.name=Test", "commit", "-m", "init")
				commitCmd.Dir = wsDir
				Expect(commitCmd.Run()).To(Succeed())
			}

			h := runner.NewUnprivilegedHandler()
			proxy := &localRunnerProxy{handler: h}

			sessResp, err := proxy.CreateSession(context.Background(), &v1.CreateSessionRequest{
				WorkingDir: wsDir,
			})
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
			ForgeConfigStore: forgeConfigStore,
			ForgeTypes: map[string]forge.Forge{
				"mock": mockForgeInst,
			},
			SessionMgr:     sessionMgr,
			Agent:          &agent.NoopAgent{},
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

	It("starts a job and returns session ID", func() {
		resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "fix the bug",
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.SessionId).NotTo(BeEmpty())
	})

	It("returns error for unknown project", func() {
		_, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project: "nonexistent",
			Prompt:  "do something",
		}))
		Expect(err).To(HaveOccurred())
	})

	It("transitions session through states to completion", func() {
		resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "fix the bug",
		}))
		Expect(err).NotTo(HaveOccurred())

		// Poll until the session reaches a terminal state.
		Eventually(func() string {
			sess, err := client.GetSession(context.Background(), connect.NewRequest(&v1.GetSessionRequest{
				SessionId: resp.Msg.SessionId,
			}))
			if err != nil {
				return ""
			}
			return sess.Msg.State
		}).Should(Equal("completed"))
	})

	It("lists sessions", func() {
		_, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "prompt 1",
		}))
		Expect(err).NotTo(HaveOccurred())

		listResp, err := client.ListSessions(context.Background(), connect.NewRequest(&v1.ListSessionsRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(listResp.Msg.Sessions).To(HaveLen(1))
	})

	It("destroys VM after job completes", func() {
		resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "fix it",
		}))
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() string {
			sess, err := client.GetSession(context.Background(), connect.NewRequest(&v1.GetSessionRequest{
				SessionId: resp.Msg.SessionId,
			}))
			if err != nil {
				return ""
			}
			return sess.Msg.State
		}).Should(Equal("completed"))
	})

	It("fails session when SCM clone fails", func() {
		mockScm.cloneErr = fmt.Errorf("clone failed")

		resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "fix it",
		}))
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() string {
			sess, err := client.GetSession(context.Background(), connect.NewRequest(&v1.GetSessionRequest{
				SessionId: resp.Msg.SessionId,
			}))
			if err != nil {
				return ""
			}
			return sess.Msg.State
		}).Should(Equal("failed"))
	})

	It("runs setup steps from config and completes", func() {
		mockScm.files = map[string][]byte{
			"kvarn.yml": []byte("setup:\n  steps:\n    - name: Install\n      run: echo installed\n"),
		}

		resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "fix the bug",
		}))
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() string {
			sess, err := client.GetSession(context.Background(), connect.NewRequest(&v1.GetSessionRequest{
				SessionId: resp.Msg.SessionId,
			}))
			if err != nil {
				return ""
			}
			return sess.Msg.State
		}).Should(Equal("completed"))
	})

	It("fails session when setup step fails", func() {
		mockScm.files = map[string][]byte{
			"kvarn.yml": []byte("setup:\n  steps:\n    - name: Fail step\n      run: exit 1\n"),
		}

		resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "fix the bug",
		}))
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() string {
			sess, err := client.GetSession(context.Background(), connect.NewRequest(&v1.GetSessionRequest{
				SessionId: resp.Msg.SessionId,
			}))
			if err != nil {
				return ""
			}
			return sess.Msg.State
		}).Should(Equal("failed"))
	})

	It("completes when required validation passes", func() {
		mockScm.files = map[string][]byte{
			"kvarn.yml": []byte("validation:\n  required:\n    - name: Tests\n      run: echo pass\n"),
		}

		resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "fix the bug",
		}))
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() string {
			sess, err := client.GetSession(context.Background(), connect.NewRequest(&v1.GetSessionRequest{
				SessionId: resp.Msg.SessionId,
			}))
			if err != nil {
				return ""
			}
			return sess.Msg.State
		}).Should(Equal("completed"))
	})

	It("fails session when required validation fails", func() {
		mockScm.files = map[string][]byte{
			"kvarn.yml": []byte("validation:\n  required:\n    - name: Tests\n      run: exit 1\n"),
		}

		resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "fix the bug",
		}))
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() string {
			sess, err := client.GetSession(context.Background(), connect.NewRequest(&v1.GetSessionRequest{
				SessionId: resp.Msg.SessionId,
			}))
			if err != nil {
				return ""
			}
			return sess.Msg.State
		}).Should(Equal("failed"))
	})

	It("completes when advisory validation fails", func() {
		mockScm.files = map[string][]byte{
			"kvarn.yml": []byte("validation:\n  advisory:\n    - name: Lint\n      run: exit 1\n"),
		}

		resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "fix the bug",
		}))
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() string {
			sess, err := client.GetSession(context.Background(), connect.NewRequest(&v1.GetSessionRequest{
				SessionId: resp.Msg.SessionId,
			}))
			if err != nil {
				return ""
			}
			return sess.Msg.State
		}).Should(Equal("completed"))
	})
})

// memSecretStore is an in-memory secret.Store for tests.
type memSecretStore struct {
	secrets map[string]map[string]*secret.Secret
}

func newMemSecretStore() *memSecretStore {
	return &memSecretStore{secrets: map[string]map[string]*secret.Secret{}}
}

func (m *memSecretStore) Get(_ context.Context, project, name string) (*secret.Secret, error) {
	if proj, ok := m.secrets[project]; ok {
		if s, ok := proj[name]; ok {
			return s, nil
		}
	}
	return nil, fmt.Errorf("secret %q not found for project %q", name, project)
}

func (m *memSecretStore) List(_ context.Context, project string) ([]*secret.Secret, error) {
	var out []*secret.Secret
	for _, s := range m.secrets[project] {
		out = append(out, s)
	}
	return out, nil
}

func (m *memSecretStore) Put(_ context.Context, s *secret.Secret) error {
	if _, ok := m.secrets[s.Project]; !ok {
		m.secrets[s.Project] = map[string]*secret.Secret{}
	}
	m.secrets[s.Project][s.Name] = s
	return nil
}

func (m *memSecretStore) Delete(_ context.Context, project, name string) error {
	if proj, ok := m.secrets[project]; ok {
		delete(proj, name)
	}
	return nil
}

var _ = Describe("StartJob with secrets", func() {
	var (
		client       kvarnv1connect.OrchestratorServiceClient
		server       *http.Server
		mockScm      *mockSCM
		sessionMgr   *session.MemoryManager
		listener     net.Listener
		tmpDir       string
		secretStore  *memSecretStore
		capturedOpts atomic.Value // sandbox.Opts
	)

	BeforeEach(func() {
		mockScm = &mockSCM{}
		sessionMgr = session.NewMemoryManager()
		secretStore = newMemSecretStore()

		var err error
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())

		tmpDir, err = os.MkdirTemp("", "secrets-test-*")
		Expect(err).NotTo(HaveOccurred())

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
				"test-forge": {Name: "test-forge", Type: "mock"},
			},
		}

		factory := func(_ context.Context, opts sandbox.Opts) (orchestrator.Sandbox, error) {
			capturedOpts.Store(opts)

			wsDir, err := os.MkdirTemp("", "test-workspace-*")
			if err != nil {
				return nil, err
			}
			if opts.SourceDir != "" {
				cpCmd := exec.Command("cp", "-a", opts.SourceDir+"/.", wsDir)
				if out, err := cpCmd.CombinedOutput(); err != nil {
					return nil, fmt.Errorf("copy source: %s: %w", out, err)
				}
			}
			if _, err := os.Stat(filepath.Join(wsDir, ".git")); os.IsNotExist(err) {
				Expect(exec.Command("git", "init", wsDir).Run()).To(Succeed())
				ac := exec.Command("git", "add", ".")
				ac.Dir = wsDir
				Expect(ac.Run()).To(Succeed())
				cc := exec.Command("git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "i")
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

		mockForgeInst := &mockForge{scmImpl: mockScm}

		svc := orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{
			CreateOpts:       vm.CreateOpts{},
			ProjectStore:     projStore,
			ForgeConfigStore: forgeConfigStore,
			ForgeTypes: map[string]forge.Forge{
				"mock": mockForgeInst,
			},
			SecretStore:    secretStore,
			SessionMgr:     sessionMgr,
			Agent:          &agent.NoopAgent{},
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

	It("injects env and bearer secrets into sandbox opts", func() {
		mockScm.files = map[string][]byte{
			"kvarn.yml": []byte("secrets:\n  - HMAC_SIGN\n  - DOCKERHUB_TOKEN\n"),
		}
		Expect(secretStore.Put(context.Background(), &secret.Secret{
			Project: "test-project", Name: "HMAC_SIGN",
			Type: secret.TypeEnv, Value: "real-hmac-value",
		})).To(Succeed())
		Expect(secretStore.Put(context.Background(), &secret.Secret{
			Project: "test-project", Name: "DOCKERHUB_TOKEN",
			Type: secret.TypeBearer, Value: "real-dockerhub-token",
		})).To(Succeed())

		resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "do",
		}))
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() string {
			s, err := client.GetSession(context.Background(), connect.NewRequest(&v1.GetSessionRequest{
				SessionId: resp.Msg.SessionId,
			}))
			if err != nil {
				return ""
			}
			return s.Msg.State
		}).Should(Equal("completed"))

		raw := capturedOpts.Load()
		Expect(raw).NotTo(BeNil())
		opts := raw.(sandbox.Opts)
		Expect(opts.Secrets).To(HaveKeyWithValue("HMAC_SIGN", "real-hmac-value"))
		Expect(opts.Secrets).To(HaveKey("DOCKERHUB_TOKEN"))
		placeholder := opts.Secrets["DOCKERHUB_TOKEN"]
		Expect(placeholder).To(HavePrefix("kvarn:"))
		Expect(placeholder).NotTo(Equal("real-dockerhub-token"))
		Expect(opts.CreateOpts.Network.SecretInjector).NotTo(BeNil())
	})

	It("fails the job when a declared secret is missing", func() {
		mockScm.files = map[string][]byte{
			"kvarn.yml": []byte("secrets:\n  - MISSING_SECRET\n"),
		}

		resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "do",
		}))
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() string {
			s, err := client.GetSession(context.Background(), connect.NewRequest(&v1.GetSessionRequest{
				SessionId: resp.Msg.SessionId,
			}))
			if err != nil {
				return ""
			}
			return s.Msg.State
		}).Should(Equal("failed"))

		s, err := client.GetSession(context.Background(), connect.NewRequest(&v1.GetSessionRequest{
			SessionId: resp.Msg.SessionId,
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(s.Msg.Error).To(ContainSubstring("MISSING_SECRET"))
	})
})

// memProjectStore is an in-memory project.Store for tests.
type memProjectStore struct {
	projects map[string]*project.Project
}

func (s *memProjectStore) Get(_ context.Context, name string) (*project.Project, error) {
	p, ok := s.projects[name]
	if !ok {
		return nil, fmt.Errorf("project %q not found", name)
	}
	return p, nil
}

func (s *memProjectStore) List(_ context.Context) ([]*project.Project, error) {
	var result []*project.Project
	for _, p := range s.projects {
		result = append(result, p)
	}
	return result, nil
}

func (s *memProjectStore) Put(_ context.Context, p *project.Project) error {
	s.projects[p.Name] = p
	return nil
}

func (s *memProjectStore) Delete(_ context.Context, name string) error {
	delete(s.projects, name)
	return nil
}

var _ = Describe("StartJob submission flow", func() {
	var (
		client        kvarnv1connect.OrchestratorServiceClient
		server        *http.Server
		mockScm       *mockSCM
		mockForgeInst *mockForge
		sessionMgr    *session.MemoryManager
		listener      net.Listener
		tmpDir        string
		testAgent     *scriptedAgent
	)

	BeforeEach(func() {
		mockScm = &mockSCM{}
		sessionMgr = session.NewMemoryManager()

		var err error
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())

		tmpDir, err = os.MkdirTemp("", "submit-test-*")
		Expect(err).NotTo(HaveOccurred())

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
				"test-forge": {
					Name:       "test-forge",
					Type:       "mock",
					Credential: "test-cred",
				},
			},
		}

		credStore := &memCredentialStore{
			creds: map[string]*credential.Credential{
				"test-cred": {
					Name:   "test-cred",
					Config: map[string]string{"token": "ghp_fake"},
				},
			},
		}

		factory := func(_ context.Context, opts sandbox.Opts) (orchestrator.Sandbox, error) {
			wsDir, err := os.MkdirTemp("", "test-workspace-*")
			if err != nil {
				return nil, err
			}
			if opts.SourceDir != "" {
				cpCmd := exec.Command("cp", "-a", opts.SourceDir+"/.", wsDir)
				if out, err := cpCmd.CombinedOutput(); err != nil {
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
			return &testSandbox{
				runner:         proxy,
				shellSessionID: sessResp.SessionId,
				workingDir:     wsDir,
			}, nil
		}

		mockForgeInst = &mockForge{scmImpl: mockScm}

		// The agent modifies an existing tracked file so `git diff
		// --name-only HEAD` flags it as a change.
		testAgent = &scriptedAgent{
			title:       "Update README greeting",
			description: "Updated the README with a friendly greeting.\n\nThe greeting is intended as a smoke-test for the submission flow.",
			fileName:    "README.md",
			fileBody:    "# Test\n\nhi there\n",
		}

		svc := orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{
			CreateOpts:       vm.CreateOpts{},
			ProjectStore:     projStore,
			CredentialStore:  credStore,
			ForgeConfigStore: forgeConfigStore,
			ForgeTypes: map[string]forge.Forge{
				"mock": mockForgeInst,
			},
			SessionMgr:     sessionMgr,
			Agent:          testAgent,
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

	It("submits a PR with commit body == PR body and posts a work-log comment", func() {
		resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "Please add a greeting file.",
		}))
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() string {
			s, err := client.GetSession(context.Background(), connect.NewRequest(&v1.GetSessionRequest{
				SessionId: resp.Msg.SessionId,
			}))
			if err != nil {
				return ""
			}
			return s.Msg.State
		}).Should(Equal("completed"))

		Expect(mockForgeInst.prCalls).To(Equal(1))
		prOpts := mockForgeInst.lastPROpts
		Expect(prOpts.Title).To(Equal(testAgent.title))
		Expect(prOpts.Body).To(Equal(testAgent.description))
		Expect(prOpts.Body).NotTo(ContainSubstring("Automatically generated"))
		Expect(prOpts.Body).NotTo(ContainSubstring("Session:"))

		Expect(mockForgeInst.commentCalls).To(Equal(1))
		commentOpts := mockForgeInst.lastCommentOpts
		Expect(commentOpts.Number).To(Equal(1))
		Expect(commentOpts.Body).To(ContainSubstring("## Task"))
		Expect(commentOpts.Body).To(ContainSubstring("Please add a greeting file."))
		Expect(commentOpts.Body).To(ContainSubstring("Work log"))
		Expect(commentOpts.Body).To(ContainSubstring("Tool: WriteFile"))
		Expect(commentOpts.Body).To(ContainSubstring("Tool failed: Bash"))
		Expect(commentOpts.Body).To(ContainSubstring("test failure: thing broke"))
	})
})
