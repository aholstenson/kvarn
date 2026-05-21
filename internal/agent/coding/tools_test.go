package coding_test

import (
	"context"
	"errors"
	"io"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	llms "github.com/aholstenson/llms-go"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/agent/coding"
	"github.com/aholstenson/kvarn/internal/agent/repocontext"
	"github.com/aholstenson/kvarn/internal/sandbox"
)

// mockRunner implements sandbox.RunnerProxy for testing.
type mockRunner struct {
	execFunc        func(ctx context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error)
	sessionExecFunc func(ctx context.Context, req *v1.SessionExecRequest) (*v1.SessionExecResponse, error)
	uploadFilesFunc func(ctx context.Context, req *v1.UploadFilesRequest) (*v1.UploadFilesResponse, error)
	readFileFunc    func(ctx context.Context, req *v1.ReadFileRequest) (*v1.ReadFileResponse, error)
	editFileFunc    func(ctx context.Context, req *v1.EditFileRequest) (*v1.EditFileResponse, error)
	writeFileFunc   func(ctx context.Context, req *v1.WriteFileRequest) (*v1.WriteFileResponse, error)
}

func (m *mockRunner) Exec(ctx context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
	if m.execFunc != nil {
		return m.execFunc(ctx, req)
	}
	return &v1.ExecResponse{}, nil
}

func (m *mockRunner) CreateSession(_ context.Context, _ *v1.CreateSessionRequest) (*v1.CreateSessionResponse, error) {
	return &v1.CreateSessionResponse{SessionId: "mock-session"}, nil
}

func (m *mockRunner) SessionExec(ctx context.Context, req *v1.SessionExecRequest, _ sandbox.OutputCallback) (*v1.SessionExecResponse, error) {
	if m.sessionExecFunc != nil {
		return m.sessionExecFunc(ctx, req)
	}
	return &v1.SessionExecResponse{}, nil
}

func (m *mockRunner) CloseSession(_ context.Context, _ *v1.CloseSessionRequest) (*v1.CloseSessionResponse, error) {
	return &v1.CloseSessionResponse{}, nil
}

func (m *mockRunner) UploadFiles(ctx context.Context, req *v1.UploadFilesRequest) (*v1.UploadFilesResponse, error) {
	return m.uploadFilesFunc(ctx, req)
}

func (m *mockRunner) ReadFile(ctx context.Context, req *v1.ReadFileRequest) (*v1.ReadFileResponse, error) {
	return m.readFileFunc(ctx, req)
}

func (m *mockRunner) EditFile(ctx context.Context, req *v1.EditFileRequest) (*v1.EditFileResponse, error) {
	return m.editFileFunc(ctx, req)
}

func (m *mockRunner) WriteFile(ctx context.Context, req *v1.WriteFileRequest) (*v1.WriteFileResponse, error) {
	return m.writeFileFunc(ctx, req)
}

func (m *mockRunner) StreamToGuest(_ context.Context, _ string, _ io.Reader, _ int64) error {
	return nil
}

func (m *mockRunner) StreamFromGuest(_ context.Context, _ string, _ io.Writer) error {
	return nil
}

var _ = Describe("CodingToolkit", func() {
	var (
		runner  *mockRunner
		toolkit *coding.CodingToolkit
		tools   map[string]llms.ToolDef
		ctx     context.Context
	)

	BeforeEach(func() {
		runner = &mockRunner{}
		toolkit = coding.NewCodingToolkit(runner, "/home/kvarn/workspace", "sess-1", nil)
		ctx = context.Background()

		tools = make(map[string]llms.ToolDef)
		for _, t := range toolkit.Tools() {
			tools[t.Name()] = t
		}
	})

	It("registers all expected tools", func() {
		Expect(tools).To(HaveKey("exec_command"))
		Expect(tools).To(HaveKey("read_file"))
		Expect(tools).To(HaveKey("edit_file"))
		Expect(tools).To(HaveKey("write_file"))
		Expect(tools).To(HaveKey("list_files"))
		Expect(tools).To(HaveKey("search_files"))
	})

	Describe("exec_command", func() {
		It("passes command and args to SessionExec", func() {
			runner.sessionExecFunc = func(_ context.Context, req *v1.SessionExecRequest) (*v1.SessionExecResponse, error) {
				Expect(req.SessionId).To(Equal("sess-1"))
				Expect(req.Command).To(Equal("go 'test' './...'"))
				return &v1.SessionExecResponse{ExitCode: 0, Stdout: "ok"}, nil
			}

			result, err := tools["exec_command"].Execute(ctx, &coding.ExecCommandInput{
				Command: "go",
				Args:    []string{"test", "./..."},
			})
			Expect(err).NotTo(HaveOccurred())
			output := result.(*coding.ExecCommandOutput)
			Expect(output.ExitCode).To(Equal(int32(0)))
			Expect(output.Stdout).To(Equal("ok"))
		})

		It("passes shell commands directly", func() {
			runner.sessionExecFunc = func(_ context.Context, req *v1.SessionExecRequest) (*v1.SessionExecResponse, error) {
				Expect(req.Command).To(Equal("cat file.txt | grep test"))
				return &v1.SessionExecResponse{ExitCode: 0, Stdout: "test line"}, nil
			}

			_, err := tools["exec_command"].Execute(ctx, &coding.ExecCommandInput{
				Command: "cat file.txt | grep test",
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("read_file", func() {
		It("forwards request and returns tagged lines", func() {
			runner.readFileFunc = func(_ context.Context, req *v1.ReadFileRequest) (*v1.ReadFileResponse, error) {
				Expect(req.WorkingDir).To(Equal("/home/kvarn/workspace"))
				Expect(req.Path).To(Equal("main.go"))
				Expect(req.StartLine).To(Equal(int32(2)))
				Expect(req.EndLine).To(Equal(int32(4)))
				return &v1.ReadFileResponse{
					Version:    "abc123",
					TotalLines: 5,
					Newline:    "\n",
					Lines: []*v1.TaggedLine{
						{Line: 2, Hash: "f1", Content: "b"},
						{Line: 3, Hash: "f2", Content: "c"},
						{Line: 4, Hash: "f3", Content: "d"},
					},
				}, nil
			}

			result, err := tools["read_file"].Execute(ctx, &coding.ReadFileInput{
				Path: "main.go", StartLine: 2, EndLine: 4,
			})
			Expect(err).NotTo(HaveOccurred())
			output := result.(*coding.ReadFileOutput)
			Expect(output.Version).To(Equal("abc123"))
			Expect(output.TotalLines).To(Equal(int32(5)))
			Expect(output.Lines).To(HaveLen(3))
			Expect(output.Lines[0].Hash).To(Equal("f1"))
		})
	})

	Describe("edit_file", func() {
		It("sends operations and returns updated context", func() {
			runner.editFileFunc = func(_ context.Context, req *v1.EditFileRequest) (*v1.EditFileResponse, error) {
				Expect(req.WorkingDir).To(Equal("/home/kvarn/workspace"))
				Expect(req.Path).To(Equal("main.go"))
				Expect(req.ExpectedVersion).To(Equal("v1"))
				Expect(req.Operations).To(HaveLen(1))
				Expect(req.Operations[0].Op).To(Equal(v1.EditOp_EDIT_OP_REPLACE))
				Expect(req.Operations[0].Line).To(Equal(int32(12)))
				Expect(req.Operations[0].Hash).To(Equal("f1"))
				Expect(req.Operations[0].Lines).To(Equal([]string{"new line"}))
				return &v1.EditFileResponse{
					Version:    "v2",
					TotalLines: 20,
					Context: []*v1.TaggedLine{
						{Line: 12, Hash: "aa", Content: "new line"},
					},
				}, nil
			}

			result, err := tools["edit_file"].Execute(ctx, &coding.EditFileInput{
				Path:            "main.go",
				ExpectedVersion: "v1",
				Operations: []coding.EditOperationInput{
					{Op: "replace", Line: 12, Hash: "f1", Lines: []string{"new line"}},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			output := result.(*coding.EditFileOutput)
			Expect(output.Version).To(Equal("v2"))
			Expect(output.Context).To(HaveLen(1))
		})

		It("renders anchor-mismatch failure with a recovery hint", func() {
			runner.editFileFunc = func(_ context.Context, _ *v1.EditFileRequest) (*v1.EditFileResponse, error) {
				return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("anchor_mismatch: operation 0 line 1 hash zz does not match current f1"))
			}
			result, err := tools["edit_file"].Execute(ctx, &coding.EditFileInput{
				Path:            "main.go",
				ExpectedVersion: "v1",
				Operations: []coding.EditOperationInput{
					{Op: "replace", Line: 1, Hash: "zz", Lines: []string{"x"}},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			output := result.(*coding.EditFileOutput)
			Expect(output.Failure).To(ContainSubstring("anchor_mismatch"))
			rendered := tools["edit_file"].ToString(output)
			Expect(rendered).To(ContainSubstring("Re-read the file to get fresh anchors"))
		})
	})

	Describe("write_file", func() {
		It("forwards create request to runner", func() {
			runner.writeFileFunc = func(_ context.Context, req *v1.WriteFileRequest) (*v1.WriteFileResponse, error) {
				Expect(req.WorkingDir).To(Equal("/home/kvarn/workspace"))
				Expect(req.Path).To(Equal("new.go"))
				Expect(string(req.Content)).To(Equal("package new\n"))
				Expect(req.ExpectedVersion).To(BeEmpty())
				return &v1.WriteFileResponse{Version: "vv", TotalLines: 1}, nil
			}

			result, err := tools["write_file"].Execute(ctx, &coding.WriteFileInput{
				Path:    "new.go",
				Content: "package new\n",
			})
			Expect(err).NotTo(HaveOccurred())
			output := result.(*coding.WriteFileOutput)
			Expect(output.Version).To(Equal("vv"))
			Expect(output.TotalLines).To(Equal(int32(1)))
		})

		It("forwards expected_version for overwrites", func() {
			runner.writeFileFunc = func(_ context.Context, req *v1.WriteFileRequest) (*v1.WriteFileResponse, error) {
				Expect(req.ExpectedVersion).To(Equal("vold"))
				return &v1.WriteFileResponse{Version: "vnew", TotalLines: 2}, nil
			}

			_, err := tools["write_file"].Execute(ctx, &coding.WriteFileInput{
				Path:            "a.txt",
				Content:         "x\n",
				ExpectedVersion: "vold",
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("list_files", func() {
		It("runs find command via runner", func() {
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				Expect(req.Command).To(Equal("find"))
				Expect(req.Args).To(ContainElement("-maxdepth"))
				Expect(req.Args).To(ContainElement("1"))
				Expect(req.WorkingDir).To(Equal("/home/kvarn/workspace"))
				return &v1.ExecResponse{ExitCode: 0, Stdout: "./main.go\n./go.mod\n"}, nil
			}

			result, err := tools["list_files"].Execute(ctx, &coding.ListFilesInput{})
			Expect(err).NotTo(HaveOccurred())
			output := result.(*coding.ListFilesOutput)
			Expect(output.Output).To(ContainSubstring("main.go"))
		})

		It("uses custom path when provided", func() {
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				Expect(req.Args[0]).To(Equal("src"))
				return &v1.ExecResponse{ExitCode: 0, Stdout: ""}, nil
			}

			_, err := tools["list_files"].Execute(ctx, &coding.ListFilesInput{Path: "src"})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("search_files", func() {
		It("runs grep command via runner", func() {
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				Expect(req.Command).To(Equal("grep"))
				Expect(req.Args).To(ContainElement("-rn"))
				Expect(req.Args).To(ContainElement("TODO"))
				Expect(req.WorkingDir).To(Equal("/home/kvarn/workspace"))
				return &v1.ExecResponse{ExitCode: 0, Stdout: "main.go:10:// TODO: fix this\n"}, nil
			}

			result, err := tools["search_files"].Execute(ctx, &coding.SearchFilesInput{Pattern: "TODO"})
			Expect(err).NotTo(HaveOccurred())
			output := result.(*coding.SearchFilesOutput)
			Expect(output.Output).To(ContainSubstring("TODO"))
		})

		It("includes glob filter when provided", func() {
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				Expect(req.Args).To(ContainElement("--include=*.go"))
				return &v1.ExecResponse{ExitCode: 0, Stdout: ""}, nil
			}

			_, err := tools["search_files"].Execute(ctx, &coding.SearchFilesInput{
				Pattern: "func",
				Glob:    "*.go",
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("activate_skill", func() {
		It("returns body in structured tags for valid skill", func() {
			skills := []repocontext.Skill{
				{
					Name:        "deploy",
					Description: "Deploy the application",
					Body:        "# Deploy\n\nRun the deploy script.\n",
					Dir:         ".agents/skills/deploy",
					Resources:   []string{"scripts/run.sh"},
				},
			}
			tkWithSkills := coding.NewCodingToolkit(runner, "/home/kvarn/workspace", "sess-1", skills)
			toolMap := make(map[string]llms.ToolDef)
			for _, t := range tkWithSkills.Tools() {
				toolMap[t.Name()] = t
			}
			Expect(toolMap).To(HaveKey("activate_skill"))

			result, err := toolMap["activate_skill"].Execute(ctx, &coding.ActivateSkillInput{Name: "deploy"})
			Expect(err).NotTo(HaveOccurred())
			output := result.(*coding.ActivateSkillOutput)
			Expect(output.Content).To(ContainSubstring(`<skill_content name="deploy">`))
			Expect(output.Content).To(ContainSubstring("# Deploy"))
			Expect(output.Content).To(ContainSubstring(".agents/skills/deploy"))
			Expect(output.Content).To(ContainSubstring("<skill_resources>"))
			Expect(output.Content).To(ContainSubstring("<file>scripts/run.sh</file>"))
			Expect(output.Content).To(ContainSubstring("</skill_content>"))
		})

		It("returns error for unknown skill name", func() {
			skills := []repocontext.Skill{
				{Name: "deploy", Description: "Deploy", Body: "body", Dir: ".agents/skills/deploy"},
			}
			tkWithSkills := coding.NewCodingToolkit(runner, "/home/kvarn/workspace", "sess-1", skills)
			toolMap := make(map[string]llms.ToolDef)
			for _, t := range tkWithSkills.Tools() {
				toolMap[t.Name()] = t
			}

			_, err := toolMap["activate_skill"].Execute(ctx, &coding.ActivateSkillInput{Name: "nonexistent"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unknown skill"))
		})

		It("omits skill_resources when no resources exist", func() {
			skills := []repocontext.Skill{
				{Name: "simple", Description: "Simple skill", Body: "Just text.\n", Dir: ".agents/skills/simple"},
			}
			tkWithSkills := coding.NewCodingToolkit(runner, "/home/kvarn/workspace", "sess-1", skills)
			toolMap := make(map[string]llms.ToolDef)
			for _, t := range tkWithSkills.Tools() {
				toolMap[t.Name()] = t
			}

			result, err := toolMap["activate_skill"].Execute(ctx, &coding.ActivateSkillInput{Name: "simple"})
			Expect(err).NotTo(HaveOccurred())
			output := result.(*coding.ActivateSkillOutput)
			Expect(output.Content).NotTo(ContainSubstring("<skill_resources>"))
		})

		It("is not registered when no skills exist", func() {
			tkNoSkills := coding.NewCodingToolkit(runner, "/home/kvarn/workspace", "sess-1", nil)
			for _, t := range tkNoSkills.Tools() {
				Expect(t.Name()).NotTo(Equal("activate_skill"))
			}
		})
	})

	Describe("ToString", func() {
		It("formats exec_command output", func() {
			result := tools["exec_command"].ToString(&coding.ExecCommandOutput{
				ExitCode: 0,
				Stdout:   "ok",
			})
			Expect(result).To(ContainSubstring("ok"))
			Expect(result).To(ContainSubstring("[exit code: 0]"))
		})

		It("formats edit_file success output", func() {
			result := tools["edit_file"].ToString(&coding.EditFileOutput{
				Version:    "vnew",
				TotalLines: 12,
				Context: []coding.TaggedLineView{
					{Line: 3, Hash: "ab", Content: "x"},
				},
			})
			Expect(result).To(ContainSubstring("Edit applied"))
			Expect(result).To(ContainSubstring("vnew"))
			Expect(result).To(ContainSubstring("3:ab|x"))
		})

		It("formats write_file success output", func() {
			result := tools["write_file"].ToString(&coding.WriteFileOutput{Version: "vv", TotalLines: 3})
			Expect(result).To(ContainSubstring("Wrote file"))
			Expect(result).To(ContainSubstring("vv"))
		})
	})
})
