package sandbox_test

import (
	"context"
	"fmt"
	"io"
	"strings"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// mockRunnerProxy records SessionExec and Exec calls and returns configurable responses.
type mockRunnerProxy struct {
	execCalls        []execCall
	sessionExecCalls []*v1.SessionExecRequest
	responses        map[string]*v1.SessionExecResponse
	// responseSequences holds per-command ordered responses for simulating
	// changing results across multiple calls (e.g. fail then succeed on retry).
	responseSequences map[string][]*v1.SessionExecResponse
	callCounts        map[string]int
	execResponses     map[string]*v1.ExecResponse
	errors            map[string]error
}

type execCall struct {
	Command    string
	Args       []string
	WorkingDir string
}

func newMockRunner() *mockRunnerProxy {
	return &mockRunnerProxy{
		responses:         make(map[string]*v1.SessionExecResponse),
		responseSequences: make(map[string][]*v1.SessionExecResponse),
		callCounts:        make(map[string]int),
		execResponses:     make(map[string]*v1.ExecResponse),
		errors:            make(map[string]error),
	}
}

func (m *mockRunnerProxy) Exec(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
	call := execCall{Command: req.Command, Args: req.Args, WorkingDir: req.WorkingDir}
	m.execCalls = append(m.execCalls, call)

	k := req.Command + " " + strings.Join(req.Args, " ")
	if err, ok := m.errors[k]; ok {
		return nil, err
	}
	if resp, ok := m.execResponses[k]; ok {
		return resp, nil
	}
	return &v1.ExecResponse{ExitCode: 0}, nil
}

func (m *mockRunnerProxy) CreateSession(_ context.Context, _ *v1.CreateSessionRequest) (*v1.CreateSessionResponse, error) {
	return &v1.CreateSessionResponse{SessionId: "mock-session"}, nil
}

func (m *mockRunnerProxy) SessionExec(_ context.Context, req *v1.SessionExecRequest, _ sandbox.OutputCallback) (*v1.SessionExecResponse, error) {
	m.sessionExecCalls = append(m.sessionExecCalls, req)

	k := req.Command
	if err, ok := m.errors[k]; ok {
		return nil, err
	}
	// If a response sequence exists for this command, return the next entry.
	if seq, ok := m.responseSequences[k]; ok && len(seq) > 0 {
		idx := m.callCounts[k]
		m.callCounts[k]++
		if idx < len(seq) {
			return seq[idx], nil
		}
		// Past the end of the sequence: fall through to the static response.
	}
	if resp, ok := m.responses[k]; ok {
		return resp, nil
	}
	return &v1.SessionExecResponse{ExitCode: 0}, nil
}

func (m *mockRunnerProxy) CloseSession(_ context.Context, _ *v1.CloseSessionRequest) (*v1.CloseSessionResponse, error) {
	return &v1.CloseSessionResponse{}, nil
}

func (m *mockRunnerProxy) UploadFiles(_ context.Context, _ *v1.UploadFilesRequest) (*v1.UploadFilesResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockRunnerProxy) ReadFile(_ context.Context, _ *v1.ReadFileRequest) (*v1.ReadFileResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockRunnerProxy) EditFile(_ context.Context, _ *v1.EditFileRequest) (*v1.EditFileResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockRunnerProxy) WriteFile(_ context.Context, _ *v1.WriteFileRequest) (*v1.WriteFileResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockRunnerProxy) StreamToGuest(_ context.Context, _ string, _ io.Reader, _ int64) error {
	return nil
}

func (m *mockRunnerProxy) StreamFromGuest(_ context.Context, _ string, _ io.Writer) error {
	return nil
}

var _ = Describe("RunSetup", func() {
	var (
		runner *mockRunnerProxy
		ctx    context.Context
	)

	BeforeEach(func() {
		runner = newMockRunner()
		ctx = context.Background()
	})

	It("runs all steps and returns results with exit codes, stdout, stderr", func() {
		runner.responses["npm install"] = &v1.SessionExecResponse{
			ExitCode: 0,
			Stdout:   "installed\n",
			Stderr:   "",
		}

		cfg := &project.Config{
			Setup: project.Setup{
				Steps: []project.Step{
					{Name: "Install", Run: "npm install"},
				},
			},
		}

		result, err := sandbox.RunSetup(ctx, runner, cfg, "sess-1", nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Steps).To(HaveLen(1))
		Expect(result.Steps[0].Name).To(Equal("Install"))
		Expect(result.Steps[0].ExitCode).To(Equal(int32(0)))
		Expect(result.Steps[0].Stdout).To(Equal("installed\n"))
	})

	It("stops on first failing step and returns error with results so far", func() {
		runner.responses["step1"] = &v1.SessionExecResponse{ExitCode: 1, Stderr: "fail"}
		runner.responses["step2"] = &v1.SessionExecResponse{ExitCode: 0}

		cfg := &project.Config{
			Setup: project.Setup{
				Steps: []project.Step{
					{Name: "Step 1", Run: "step1"},
					{Name: "Step 2", Run: "step2"},
				},
			},
		}

		result, err := sandbox.RunSetup(ctx, runner, cfg, "sess-1", nil, nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("Step 1"))
		Expect(result.Steps).To(HaveLen(1))
	})

	It("returns error when health check fails after setup succeeds", func() {
		runner.responses["npm install"] = &v1.SessionExecResponse{ExitCode: 0}
		runner.responses["pg_isready"] = &v1.SessionExecResponse{ExitCode: 2, Stderr: "not ready"}

		cfg := &project.Config{
			Setup: project.Setup{
				Steps: []project.Step{
					{Name: "Install", Run: "npm install"},
				},
				HealthChecks: []project.Step{
					{Name: "DB ready", Run: "pg_isready"},
				},
			},
		}

		result, err := sandbox.RunSetup(ctx, runner, cfg, "sess-1", nil, nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("DB ready"))
		Expect(result.Steps).To(HaveLen(1))
		Expect(result.Steps[0].ExitCode).To(Equal(int32(0)))
		Expect(result.HealthChecks).To(HaveLen(1))
		Expect(result.HealthChecks[0].ExitCode).To(Equal(int32(2)))
	})

	It("succeeds immediately for empty config", func() {
		cfg := &project.Config{}
		result, err := sandbox.RunSetup(ctx, runner, cfg, "sess-1", nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Steps).To(BeEmpty())
		Expect(result.HealthChecks).To(BeEmpty())
		Expect(runner.sessionExecCalls).To(BeEmpty())
	})

	It("succeeds for nil config", func() {
		result, err := sandbox.RunSetup(ctx, runner, nil, "sess-1", nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Steps).To(BeEmpty())
		Expect(runner.sessionExecCalls).To(BeEmpty())
	})

	It("prepends cd for step with working_dir", func() {
		cfg := &project.Config{
			Setup: project.Setup{
				Steps: []project.Step{
					{Name: "Build frontend", Run: "npm run build", WorkingDir: "frontend"},
				},
			},
		}

		sandbox.RunSetup(ctx, runner, cfg, "sess-1", nil, nil)
		Expect(runner.sessionExecCalls).To(HaveLen(1))
		Expect(runner.sessionExecCalls[0].Command).To(ContainSubstring(`cd 'frontend'`))
		Expect(runner.sessionExecCalls[0].Command).To(ContainSubstring("npm run build"))
	})

	It("retries a step that fails once and succeeds on the retry", func() {
		// First call returns failure, second returns success.
		runner.responseSequences["npm install"] = []*v1.SessionExecResponse{
			{ExitCode: 1, Stderr: "transient error"},
			{ExitCode: 0, Stdout: "installed\n"},
		}

		cfg := &project.Config{
			Setup: project.Setup{
				Steps: []project.Step{
					{Name: "Install", Run: "npm install", Retry: 1},
				},
			},
		}

		result, err := sandbox.RunSetup(ctx, runner, cfg, "sess-1", nil, nil)
		Expect(err).NotTo(HaveOccurred())
		// Only the final (successful) result is recorded.
		Expect(result.Steps).To(HaveLen(1))
		Expect(result.Steps[0].ExitCode).To(Equal(int32(0)))
		// Two attempts were made (1 initial + 1 retry).
		Expect(runner.sessionExecCalls).To(HaveLen(2))
	})

	It("fails after exhausting all retries", func() {
		// All calls return failure.
		runner.responses["npm install"] = &v1.SessionExecResponse{ExitCode: 1, Stderr: "always fails"}

		cfg := &project.Config{
			Setup: project.Setup{
				Steps: []project.Step{
					{Name: "Install", Run: "npm install", Retry: 2},
				},
			},
		}

		result, err := sandbox.RunSetup(ctx, runner, cfg, "sess-1", nil, nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("Install"))
		// Only the final result is recorded.
		Expect(result.Steps).To(HaveLen(1))
		Expect(result.Steps[0].ExitCode).To(Equal(int32(1)))
		// Three attempts were made (1 initial + 2 retries).
		Expect(runner.sessionExecCalls).To(HaveLen(3))
	})

	It("does not retry when retry is 0 (default)", func() {
		runner.responses["npm install"] = &v1.SessionExecResponse{ExitCode: 1}

		cfg := &project.Config{
			Setup: project.Setup{
				Steps: []project.Step{
					{Name: "Install", Run: "npm install", Retry: 0},
				},
			},
		}

		sandbox.RunSetup(ctx, runner, cfg, "sess-1", nil, nil)
		// Exactly one attempt made.
		Expect(runner.sessionExecCalls).To(HaveLen(1))
	})

	It("does not retry health checks even when retry is set", func() {
		// Health check always fails; retry on the step struct should be ignored.
		runner.responses["pg_isready"] = &v1.SessionExecResponse{ExitCode: 1, Stderr: "not ready"}

		cfg := &project.Config{
			Setup: project.Setup{
				HealthChecks: []project.Step{
					{Name: "DB ready", Run: "pg_isready", Retry: 3},
				},
			},
		}

		sandbox.RunSetup(ctx, runner, cfg, "sess-1", nil, nil)
		// Health checks are not retried, so exactly one call is made.
		Expect(runner.sessionExecCalls).To(HaveLen(1))
	})
})

var _ = Describe("RunValidation", func() {
	var (
		runner *mockRunnerProxy
		ctx    context.Context
	)

	BeforeEach(func() {
		runner = newMockRunner()
		ctx = context.Background()
	})

	It("returns RequiredPassed true when all required pass", func() {
		cfg := &project.Config{
			Validation: project.Validation{
				Required: []project.Step{
					{Name: "Test 1", Run: "test1"},
					{Name: "Test 2", Run: "test2"},
				},
			},
		}

		result, err := sandbox.RunValidation(ctx, runner, cfg, "sess-1", nil, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequiredPassed).To(BeTrue())
		Expect(result.Required).To(HaveLen(2))
	})

	It("runs all required steps even when one fails and sets RequiredPassed false", func() {
		runner.responses["test1"] = &v1.SessionExecResponse{ExitCode: 1}
		runner.responses["test2"] = &v1.SessionExecResponse{ExitCode: 0}

		cfg := &project.Config{
			Validation: project.Validation{
				Required: []project.Step{
					{Name: "Test 1", Run: "test1"},
					{Name: "Test 2", Run: "test2"},
				},
			},
		}

		result, err := sandbox.RunValidation(ctx, runner, cfg, "sess-1", nil, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequiredPassed).To(BeFalse())
		Expect(result.Required).To(HaveLen(2))
		Expect(result.Required[0].ExitCode).To(Equal(int32(1)))
		Expect(result.Required[1].ExitCode).To(Equal(int32(0)))
	})

	It("advisory failure does not affect RequiredPassed", func() {
		runner.responses["lint"] = &v1.SessionExecResponse{ExitCode: 1}

		cfg := &project.Config{
			Validation: project.Validation{
				Required: []project.Step{
					{Name: "Tests", Run: "tests"},
				},
				Advisory: []project.Step{
					{Name: "Lint", Run: "lint"},
				},
			},
		}

		result, err := sandbox.RunValidation(ctx, runner, cfg, "sess-1", nil, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequiredPassed).To(BeTrue())
		Expect(result.Advisory).To(HaveLen(1))
		Expect(result.Advisory[0].ExitCode).To(Equal(int32(1)))
	})

	It("skips step when paths don't match changed files", func() {
		cfg := &project.Config{
			Validation: project.Validation{
				Required: []project.Step{
					{Name: "Backend tests", Run: "phpunit", Paths: []string{"backend/**"}},
				},
			},
		}

		result, err := sandbox.RunValidation(ctx, runner, cfg, "sess-1", []string{"frontend/app.ts"}, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Required).To(HaveLen(1))
		Expect(result.Required[0].Skipped).To(BeTrue())
		Expect(result.RequiredPassed).To(BeTrue())
		Expect(runner.sessionExecCalls).To(BeEmpty())
	})

	It("runs step when paths match changed files", func() {
		cfg := &project.Config{
			Validation: project.Validation{
				Required: []project.Step{
					{Name: "Backend tests", Run: "phpunit", Paths: []string{"backend/**"}},
				},
			},
		}

		result, err := sandbox.RunValidation(ctx, runner, cfg, "sess-1", []string{"backend/src/foo.php"}, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Required).To(HaveLen(1))
		Expect(result.Required[0].Skipped).To(BeFalse())
		Expect(runner.sessionExecCalls).To(HaveLen(1))
	})

	It("always runs step with no paths regardless of changed files", func() {
		cfg := &project.Config{
			Validation: project.Validation{
				Required: []project.Step{
					{Name: "Integration", Run: "integration"},
				},
			},
		}

		result, err := sandbox.RunValidation(ctx, runner, cfg, "sess-1", []string{"unrelated/file.txt"}, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Required[0].Skipped).To(BeFalse())
		Expect(runner.sessionExecCalls).To(HaveLen(1))
	})

	It("returns success immediately for empty config", func() {
		cfg := &project.Config{}
		result, err := sandbox.RunValidation(ctx, runner, cfg, "sess-1", nil, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequiredPassed).To(BeTrue())
		Expect(result.Required).To(BeEmpty())
		Expect(result.Advisory).To(BeEmpty())
		Expect(runner.sessionExecCalls).To(BeEmpty())
	})
})

var _ = Describe("Step execution", func() {
	It("uses SessionExec to run commands", func() {
		runner := newMockRunner()
		ctx := context.Background()

		cfg := &project.Config{
			Setup: project.Setup{
				Steps: []project.Step{
					{Name: "Build", Run: "go build ./..."},
				},
			},
		}

		_, err := sandbox.RunSetup(ctx, runner, cfg, "sess-1", nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(runner.sessionExecCalls).To(HaveLen(1))
		call := runner.sessionExecCalls[0]
		Expect(call.SessionId).To(Equal("sess-1"))
		Expect(call.Command).To(Equal("go build ./..."))
	})
})

var _ = Describe("PullImage", func() {
	It("sends podman pull command", func() {
		runner := newMockRunner()
		err := sandbox.PullImage(context.Background(), runner, "node:20")
		Expect(err).NotTo(HaveOccurred())
		Expect(runner.execCalls).To(HaveLen(1))
		Expect(runner.execCalls[0].Command).To(Equal("podman"))
		Expect(runner.execCalls[0].Args).To(Equal([]string{"pull", "node:20"}))
	})

	It("returns error on non-zero exit", func() {
		runner := newMockRunner()
		runner.execResponses["podman pull node:20"] = &v1.ExecResponse{ExitCode: 1, Stderr: "not found"}
		err := sandbox.PullImage(context.Background(), runner, "node:20")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("failed"))
	})
})

var _ = Describe("ShouldRun (via RunValidation)", func() {
	var (
		runner *mockRunnerProxy
		ctx    context.Context
	)

	BeforeEach(func() {
		runner = newMockRunner()
		ctx = context.Background()
	})

	It("returns true for empty paths", func() {
		cfg := &project.Config{
			Validation: project.Validation{
				Required: []project.Step{
					{Name: "Test", Run: "test"},
				},
			},
		}
		result, err := sandbox.RunValidation(ctx, runner, cfg, "sess-1", []string{"any/file.go"}, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Required[0].Skipped).To(BeFalse())
	})

	It("matches doublestar patterns like **/*.go", func() {
		cfg := &project.Config{
			Validation: project.Validation{
				Required: []project.Step{
					{Name: "Test", Run: "test", Paths: []string{"**/*.go"}},
				},
			},
		}
		result, err := sandbox.RunValidation(ctx, runner, cfg, "sess-1", []string{"internal/pkg/main.go"}, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Required[0].Skipped).To(BeFalse())

		result, err = sandbox.RunValidation(ctx, runner, cfg, "sess-1", []string{"README.md"}, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Required[0].Skipped).To(BeTrue())
	})
})

var _ = Describe("ChangedFiles", func() {
	It("parses git diff output into file list", func() {
		runner := newMockRunner()
		runner.execResponses["git diff --name-only HEAD"] = &v1.ExecResponse{
			ExitCode: 0,
			Stdout:   "src/main.go\ninternal/pkg/util.go\nREADME.md\n",
		}

		files, err := sandbox.ChangedFiles(context.Background(), runner, "/home/kvarn/workspace")
		Expect(err).NotTo(HaveOccurred())
		Expect(files).To(Equal([]string{"src/main.go", "internal/pkg/util.go", "README.md"}))
	})

	It("returns empty list for empty output", func() {
		runner := newMockRunner()
		runner.execResponses["git diff --name-only HEAD"] = &v1.ExecResponse{
			ExitCode: 0,
			Stdout:   "",
		}

		files, err := sandbox.ChangedFiles(context.Background(), runner, "/home/kvarn/workspace")
		Expect(err).NotTo(HaveOccurred())
		Expect(files).To(BeEmpty())
	})

	It("returns error when exec fails", func() {
		runner := newMockRunner()
		runner.errors["git diff --name-only HEAD"] = fmt.Errorf("connection lost")

		_, err := sandbox.ChangedFiles(context.Background(), runner, "/home/kvarn/workspace")
		Expect(err).To(HaveOccurred())
	})
})
