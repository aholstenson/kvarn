package preview_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/preview"
	"github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox"
)

// stubProcesses records what was started and hands back the callbacks so a spec
// can drive output and exits.
type stubProcesses struct {
	mu       sync.Mutex
	started  []*v1.StartProcessRequest
	onOutput map[string]sandbox.OutputCallback
	onExit   map[string]sandbox.ProcessExitCallback
	err      error
}

func newStubProcesses() *stubProcesses {
	return &stubProcesses{
		onOutput: map[string]sandbox.OutputCallback{},
		onExit:   map[string]sandbox.ProcessExitCallback{},
	}
}

func (s *stubProcesses) StartProcess(_ context.Context, req *v1.StartProcessRequest, onOutput sandbox.OutputCallback, onExit sandbox.ProcessExitCallback) (*v1.StartProcessResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	s.started = append(s.started, req)
	s.onOutput[req.Name] = onOutput
	s.onExit[req.Name] = onExit
	return &v1.StartProcessResponse{}, nil
}

func (s *stubProcesses) StopProcess(context.Context, *v1.StopProcessRequest) (*v1.StopProcessResponse, error) {
	return nil, errors.ErrUnsupported
}

func (s *stubProcesses) ListProcesses(context.Context, *v1.ListProcessesRequest) (*v1.ListProcessesResponse, error) {
	return nil, errors.ErrUnsupported
}

// stubRunner answers SessionExec from a queue of responses, so a ready check can
// be made to fail before it passes. Everything else is unsupported.
type stubRunner struct {
	mu        sync.Mutex
	responses []*v1.SessionExecResponse
	commands  []string
}

func (s *stubRunner) SessionExec(_ context.Context, req *v1.SessionExecRequest, _ sandbox.OutputCallback) (*v1.SessionExecResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, req.Command)
	if len(s.responses) == 0 {
		return &v1.SessionExecResponse{}, nil
	}
	resp := s.responses[0]
	s.responses = s.responses[1:]
	return resp, nil
}

func (s *stubRunner) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.commands...)
}

func (s *stubRunner) Exec(context.Context, *v1.ExecRequest) (*v1.ExecResponse, error) {
	return nil, errors.ErrUnsupported
}
func (s *stubRunner) CreateSession(context.Context, *v1.CreateSessionRequest) (*v1.CreateSessionResponse, error) {
	return nil, errors.ErrUnsupported
}
func (s *stubRunner) CloseSession(context.Context, *v1.CloseSessionRequest) (*v1.CloseSessionResponse, error) {
	return nil, errors.ErrUnsupported
}
func (s *stubRunner) UploadFiles(context.Context, *v1.UploadFilesRequest) (*v1.UploadFilesResponse, error) {
	return nil, errors.ErrUnsupported
}
func (s *stubRunner) ReadFile(context.Context, *v1.ReadFileRequest) (*v1.ReadFileResponse, error) {
	return nil, errors.ErrUnsupported
}
func (s *stubRunner) EditFile(context.Context, *v1.EditFileRequest) (*v1.EditFileResponse, error) {
	return nil, errors.ErrUnsupported
}
func (s *stubRunner) WriteFile(context.Context, *v1.WriteFileRequest) (*v1.WriteFileResponse, error) {
	return nil, errors.ErrUnsupported
}
func (s *stubRunner) StreamToGuest(context.Context, string, io.Reader, int64) error {
	return errors.ErrUnsupported
}
func (s *stubRunner) StreamFromGuest(context.Context, string, io.Writer) error {
	return errors.ErrUnsupported
}

var _ = Describe("StartServices", func() {
	var cfg *project.Config

	BeforeEach(func() {
		cfg = &project.Config{Preview: project.Preview{
			Sites: map[string]project.PreviewSite{
				"web": {Port: 3000},
				"api": {Port: 8080},
			},
			Serve: []project.PreviewProcess{
				{Name: "web server", Run: "npm start", Port: 3000},
				{Name: "api server", Run: "go run ./cmd/api", Port: 8080, WorkingDir: "backend"},
			},
		}}
	})

	It("starts every serve step with all site URLs in its environment", func() {
		procs := newStubProcesses()
		err := preview.StartServices(context.Background(), procs, cfg, preview.ServeOpts{
			WorkspaceDir: "/home/kvarn/workspace",
			URLs:         map[string]string{"web": "http://localhost:3000", "api": "http://localhost:8080"},
			IDPrefix:     "local",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(procs.started).To(HaveLen(2))

		web := procs.started[0]
		Expect(web.Name).To(Equal("web server"))
		Expect(web.ProcessId).To(Equal("local/port-3000"))
		Expect(web.WorkingDir).To(Equal("/home/kvarn/workspace"))
		// Every process sees every site's URL, not just the ones on its own
		// port: sites in one preview have to be able to link to each other, and
		// a server hosting several needs all of their names.
		Expect(web.Env).To(HaveKeyWithValue("KVARN_PREVIEW_URL_WEB", "http://localhost:3000"))
		Expect(web.Env).To(HaveKeyWithValue("KVARN_PREVIEW_URL_API", "http://localhost:8080"))

		Expect(procs.started[1].WorkingDir).To(Equal("/home/kvarn/workspace/backend"))
	})

	It("reports which serve step could not be started", func() {
		procs := newStubProcesses()
		procs.err = errors.New("no such command")
		err := preview.StartServices(context.Background(), procs, cfg, preview.ServeOpts{})
		Expect(err).To(MatchError(ContainSubstring(`start serve step "web server"`)))
	})

	It("refuses a sandbox that cannot run long-lived processes", func() {
		err := preview.StartServices(context.Background(), nil, cfg, preview.ServeOpts{})
		Expect(err).To(MatchError(ContainSubstring("long-lived processes")))
	})

	It("routes output and exits to the callbacks, named by service", func() {
		procs := newStubProcesses()
		var gotOutput, gotExit string
		Expect(preview.StartServices(context.Background(), procs, cfg, preview.ServeOpts{
			OnOutput: func(name, stdout, _ string) { gotOutput = name + ": " + stdout },
			OnExit:   func(name string, code int32, _ error) { gotExit = name + ": " + preview.FormatExitCode(code) },
		})).To(Succeed())

		procs.onOutput["web server"]("listening\n", "")
		procs.onExit["web server"](137, nil)
		Expect(gotOutput).To(Equal("web server: listening\n"))
		Expect(gotExit).To(Equal("web server: 137 (signal 9)"))
	})
})

var _ = Describe("WaitReady", func() {
	It("does nothing when the repository declares no checks", func() {
		runner := &stubRunner{}
		Expect(preview.WaitReady(context.Background(), runner, "shell", nil, nil)).To(Succeed())
		Expect(runner.calls()).To(BeEmpty())
	})

	It("runs a check in its working directory and reports it passing", func() {
		runner := &stubRunner{}
		var passed []string
		err := preview.WaitReady(context.Background(), runner, "shell",
			[]project.Step{{Name: "http", Run: "curl -fsS localhost:3000", WorkingDir: "web"}},
			func(name string) { passed = append(passed, name) })
		Expect(err).NotTo(HaveOccurred())
		Expect(runner.calls()).To(Equal([]string{"cd 'web' && curl -fsS localhost:3000"}))
		Expect(passed).To(Equal([]string{"http"}))
	})

	It("retries a check that has not come up yet", func() {
		runner := &stubRunner{responses: []*v1.SessionExecResponse{
			{ExitCode: 7, Stderr: "connection refused"},
			{ExitCode: 0},
		}}
		err := preview.WaitReady(context.Background(), runner, "shell",
			[]project.Step{{Name: "http", Run: "curl -fsS localhost:3000"}}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(runner.calls()).To(HaveLen(2))
	})

	It("gives up when the context is cancelled rather than serving out the retries", func() {
		runner := &stubRunner{responses: []*v1.SessionExecResponse{{ExitCode: 1}}}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- preview.WaitReady(ctx, runner, "shell",
				[]project.Step{{Name: "http", Run: "false"}}, nil)
		}()
		Eventually(runner.calls).Should(HaveLen(1))
		cancel()
		Eventually(done, 2*time.Second).Should(Receive(MatchError(context.Canceled)))
	})
})
