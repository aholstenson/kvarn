package coding_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

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
		Expect(tools).To(HaveKey("add_task"))
		Expect(tools).To(HaveKey("update_task"))
		Expect(tools).To(HaveKey("list_tasks"))
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

		It("gives the command a default timeout in the guest", func() {
			var got uint32
			runner.sessionExecFunc = func(_ context.Context, req *v1.SessionExecRequest) (*v1.SessionExecResponse, error) {
				got = req.TimeoutSeconds
				return &v1.SessionExecResponse{}, nil
			}

			_, err := tools["exec_command"].Execute(ctx, &coding.ExecCommandInput{Command: "make"})
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(uint32(600)))
		})

		It("honours a requested timeout up to the ceiling", func() {
			var got uint32
			runner.sessionExecFunc = func(_ context.Context, req *v1.SessionExecRequest) (*v1.SessionExecResponse, error) {
				got = req.TimeoutSeconds
				return &v1.SessionExecResponse{}, nil
			}

			_, err := tools["exec_command"].Execute(ctx, &coding.ExecCommandInput{
				Command:        "make",
				TimeoutSeconds: 900,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(uint32(900)))

			_, err = tools["exec_command"].Execute(ctx, &coding.ExecCommandInput{
				Command:        "make",
				TimeoutSeconds: 99999,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(uint32(1800)))
		})

		It("renders a timeout as a timeout rather than as an exit code", func() {
			runner.sessionExecFunc = func(_ context.Context, _ *v1.SessionExecRequest) (*v1.SessionExecResponse, error) {
				return &v1.SessionExecResponse{
					ExitCode: 124,
					Stdout:   "running tests\n",
					TimedOut: true,
				}, nil
			}

			result, err := tools["exec_command"].Execute(ctx, &coding.ExecCommandInput{
				Command:        "vendor/bin/paratest",
				TimeoutSeconds: 30,
			})
			Expect(err).NotTo(HaveOccurred())

			rendered := tools["exec_command"].Render(result).Text
			Expect(rendered).To(ContainSubstring("running tests"))
			Expect(rendered).To(ContainSubstring("timed out after 30s"))
			Expect(rendered).NotTo(ContainSubstring("exit code"))
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
						{Line: 2, Hash: "cedar", Content: "b"},
						{Line: 3, Hash: "maple", Content: "c"},
						{Line: 4, Hash: "birch", Content: "d"},
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
			Expect(output.Lines[0].Hash).To(Equal("cedar"))
		})

		It("bounds a read that asks for no particular window", func() {
			runner.readFileFunc = func(_ context.Context, req *v1.ReadFileRequest) (*v1.ReadFileResponse, error) {
				Expect(req.StartLine).To(Equal(int32(1)))
				Expect(req.EndLine).To(Equal(int32(2000)))
				return &v1.ReadFileResponse{}, nil
			}

			_, err := tools["read_file"].Execute(ctx, &coding.ReadFileInput{Path: "main.go"})
			Expect(err).NotTo(HaveOccurred())
		})

		It("bounds a read that names a start but no end", func() {
			runner.readFileFunc = func(_ context.Context, req *v1.ReadFileRequest) (*v1.ReadFileResponse, error) {
				Expect(req.StartLine).To(Equal(int32(500)))
				Expect(req.EndLine).To(Equal(int32(2499)))
				return &v1.ReadFileResponse{}, nil
			}

			_, err := tools["read_file"].Execute(ctx, &coding.ReadFileInput{Path: "main.go", StartLine: 500})
			Expect(err).NotTo(HaveOccurred())
		})

		It("shortens a window longer than the ceiling", func() {
			runner.readFileFunc = func(_ context.Context, req *v1.ReadFileRequest) (*v1.ReadFileResponse, error) {
				Expect(req.StartLine).To(Equal(int32(1)))
				Expect(req.EndLine).To(Equal(int32(2000)))
				return &v1.ReadFileResponse{}, nil
			}

			_, err := tools["read_file"].Execute(ctx, &coding.ReadFileInput{
				Path: "main.go", StartLine: 1, EndLine: 99999,
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("says where to continue when the window stops short of the file", func() {
			runner.readFileFunc = func(_ context.Context, _ *v1.ReadFileRequest) (*v1.ReadFileResponse, error) {
				return &v1.ReadFileResponse{
					Version:    "abc123",
					TotalLines: 12043,
					Lines: []*v1.TaggedLine{
						{Line: 1, Hash: "cedar", Content: "package main"},
						{Line: 2000, Hash: "maple", Content: "}"},
					},
				}, nil
			}

			result, err := tools["read_file"].Execute(ctx, &coding.ReadFileInput{Path: "big.go"})
			Expect(err).NotTo(HaveOccurred())

			text := tools["read_file"].Render(result).Text
			Expect(text).To(ContainSubstring("total_lines: 12043"))
			Expect(text).To(ContainSubstring("showing lines 1-2000 of 12043"))
			Expect(text).To(ContainSubstring("Continue with start_line=2001"))
		})

		It("says nothing extra when the whole file was returned", func() {
			runner.readFileFunc = func(_ context.Context, _ *v1.ReadFileRequest) (*v1.ReadFileResponse, error) {
				return &v1.ReadFileResponse{
					Version:    "abc123",
					TotalLines: 2,
					Lines: []*v1.TaggedLine{
						{Line: 1, Hash: "cedar", Content: "a"},
						{Line: 2, Hash: "maple", Content: "b"},
					},
				}, nil
			}

			result, err := tools["read_file"].Execute(ctx, &coding.ReadFileInput{Path: "small.go"})
			Expect(err).NotTo(HaveOccurred())
			Expect(tools["read_file"].Render(result).Text).NotTo(ContainSubstring("kvarn:"))
		})

		It("marks a window that ends at the file's end but starts past its beginning", func() {
			runner.readFileFunc = func(_ context.Context, _ *v1.ReadFileRequest) (*v1.ReadFileResponse, error) {
				return &v1.ReadFileResponse{
					Version:    "abc123",
					TotalLines: 300,
					Lines: []*v1.TaggedLine{
						{Line: 299, Hash: "cedar", Content: "a"},
						{Line: 300, Hash: "maple", Content: "b"},
					},
				}, nil
			}

			result, err := tools["read_file"].Execute(ctx, &coding.ReadFileInput{
				Path: "mid.go", StartLine: 299,
			})
			Expect(err).NotTo(HaveOccurred())

			text := tools["read_file"].Render(result).Text
			Expect(text).To(ContainSubstring("showing lines 299-300 of 300"))
			Expect(text).NotTo(ContainSubstring("Continue with"))
		})

		It("renders an empty file without a window note", func() {
			runner.readFileFunc = func(_ context.Context, _ *v1.ReadFileRequest) (*v1.ReadFileResponse, error) {
				return &v1.ReadFileResponse{Version: "abc123", TotalLines: 0}, nil
			}

			result, err := tools["read_file"].Execute(ctx, &coding.ReadFileInput{Path: "empty.go"})
			Expect(err).NotTo(HaveOccurred())
			Expect(tools["read_file"].Render(result).Text).NotTo(ContainSubstring("kvarn:"))
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
				Expect(req.Operations[0].Hash).To(Equal("cedar"))
				Expect(req.Operations[0].Lines).To(Equal([]string{"new line"}))
				return &v1.EditFileResponse{
					Version:    "v2",
					TotalLines: 20,
					Context: []*v1.TaggedLine{
						{Line: 12, Hash: "oakwood", Content: "new line"},
					},
				}, nil
			}

			result, err := tools["edit_file"].Execute(ctx, &coding.EditFileInput{
				Path:            "main.go",
				ExpectedVersion: "v1",
				Operations: []coding.EditOperationInput{
					{Op: "replace", Line: 12, Hash: "cedar", Lines: []string{"new line"}},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			output := result.(*coding.EditFileOutput)
			Expect(output.Version).To(Equal("v2"))
			Expect(output.Context).To(HaveLen(1))
		})

		It("sends insert_before operation", func() {
			runner.editFileFunc = func(_ context.Context, req *v1.EditFileRequest) (*v1.EditFileResponse, error) {
				Expect(req.Operations).To(HaveLen(1))
				Expect(req.Operations[0].Op).To(Equal(v1.EditOp_EDIT_OP_INSERT_BEFORE))
				Expect(req.Operations[0].Hash).To(Equal("cedar"))
				Expect(req.Operations[0].Lines).To(Equal([]string{"header"}))
				return &v1.EditFileResponse{Version: "v2", TotalLines: 21}, nil
			}

			_, err := tools["edit_file"].Execute(ctx, &coding.EditFileInput{
				Path: "main.go",
				Operations: []coding.EditOperationInput{
					{Op: "insert_before", Hash: "cedar", Lines: []string{"header"}},
				},
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("surfaces version_drift in the rendered output", func() {
			runner.editFileFunc = func(_ context.Context, _ *v1.EditFileRequest) (*v1.EditFileResponse, error) {
				return &v1.EditFileResponse{Version: "v2", TotalLines: 5, VersionDrift: true}, nil
			}
			result, err := tools["edit_file"].Execute(ctx, &coding.EditFileInput{
				Path:            "main.go",
				ExpectedVersion: "vstale",
				Operations: []coding.EditOperationInput{
					{Op: "replace", Hash: "cedar", Lines: []string{"x"}},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			rendered := tools["edit_file"].Render(result.(*coding.EditFileOutput)).Text
			Expect(rendered).To(ContainSubstring("changed elsewhere"))
		})

		It("renders anchor-mismatch failure with a recovery hint", func() {
			runner.editFileFunc = func(_ context.Context, _ *v1.EditFileRequest) (*v1.EditFileResponse, error) {
				return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("anchor_mismatch: anchor \"cedar\" matches no line"))
			}
			result, err := tools["edit_file"].Execute(ctx, &coding.EditFileInput{
				Path:            "main.go",
				ExpectedVersion: "v1",
				Operations: []coding.EditOperationInput{
					{Op: "replace", Hash: "cedar", Lines: []string{"x"}},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			output := result.(*coding.EditFileOutput)
			Expect(output.Failure).To(ContainSubstring("anchor_mismatch"))
			rendered := tools["edit_file"].Render(output).Text
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
		It("walks one level of the workspace root by default", func() {
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				Expect(req.Command).To(HavePrefix("find '.' -mindepth 1 -maxdepth 1 "))
				Expect(req.Command).To(HaveSuffix("| head -n 501"))
				Expect(req.WorkingDir).To(Equal("/home/kvarn/workspace"))
				return &v1.ExecResponse{Stdout: "f ./main.go\nf ./go.mod\n"}, nil
			}

			result, err := tools["list_files"].Execute(ctx, &coding.ListFilesInput{})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.(*coding.ListFilesOutput).Entries).To(Equal([]string{"main.go", "go.mod"}))
		})

		It("marks directories, symlinks and the directories it does not descend into", func() {
			runner.execFunc = func(_ context.Context, _ *v1.ExecRequest) (*v1.ExecResponse, error) {
				return &v1.ExecResponse{Stdout: "d ./internal\nl ./link\nf ./go.mod\np ./node_modules\np ./.git\n"}, nil
			}

			result, err := tools["list_files"].Execute(ctx, &coding.ListFilesInput{})
			Expect(err).NotTo(HaveOccurred())
			Expect(tools["list_files"].Render(result).Text).To(Equal(
				"internal/\nlink@\ngo.mod\nnode_modules/ (skipped)\n.git/ (skipped)\n"))
		})

		It("prunes dependency and version-control machinery", func() {
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				Expect(req.Command).To(ContainSubstring(`\( -name '.git' -o -name 'node_modules'`))
				Expect(req.Command).To(ContainSubstring(`-name '.venv'`))
				Expect(req.Command).To(ContainSubstring(`\) -prune -printf 'p %p\n' -o -printf '%y %p\n'`))
				return &v1.ExecResponse{}, nil
			}

			_, err := tools["list_files"].Execute(ctx, &coding.ListFilesInput{})
			Expect(err).NotTo(HaveOccurred())
		})

		It("uses custom path and depth when provided", func() {
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				Expect(req.Command).To(HavePrefix("find 'src' -mindepth 1 -maxdepth 3 "))
				return &v1.ExecResponse{}, nil
			}

			_, err := tools["list_files"].Execute(ctx, &coding.ListFilesInput{Path: "src", MaxDepth: 3})
			Expect(err).NotTo(HaveOccurred())
		})

		It("spells a dash-leading path so find reads it as a path", func() {
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				Expect(req.Command).To(HavePrefix("find './-weird' "))
				return &v1.ExecResponse{}, nil
			}

			_, err := tools["list_files"].Execute(ctx, &coding.ListFilesInput{Path: "-weird"})
			Expect(err).NotTo(HaveOccurred())
		})

		It("caps max_entries at the ceiling", func() {
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				Expect(req.Command).To(HaveSuffix("| head -n 2001"))
				return &v1.ExecResponse{}, nil
			}

			_, err := tools["list_files"].Execute(ctx, &coding.ListFilesInput{MaxEntries: 99999})
			Expect(err).NotTo(HaveOccurred())
		})

		It("says how many entries it left out", func() {
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				if strings.HasSuffix(req.Command, "| wc -l") {
					Expect(req.Command).To(ContainSubstring("-prune -print -o -print"))
					return &v1.ExecResponse{Stdout: "  48210\n"}, nil
				}
				var sb strings.Builder
				for i := range 501 {
					fmt.Fprintf(&sb, "f ./file%d.go\n", i)
				}
				return &v1.ExecResponse{Stdout: sb.String()}, nil
			}

			result, err := tools["list_files"].Execute(ctx, &coding.ListFilesInput{})
			Expect(err).NotTo(HaveOccurred())

			output := result.(*coding.ListFilesOutput)
			Expect(output.Entries).To(HaveLen(500))
			Expect(output.Overflow.TotalEntries).To(Equal(48210))
			Expect(tools["list_files"].Render(result).Text).To(ContainSubstring("showing 500 of 48210 entries"))
		})

		It("keeps the entries when the count pass fails", func() {
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				if strings.HasSuffix(req.Command, "| wc -l") {
					return nil, errors.New("runner is gone")
				}
				return &v1.ExecResponse{Stdout: strings.Repeat("f ./x.go\n", 501)}, nil
			}

			result, err := tools["list_files"].Execute(ctx, &coding.ListFilesInput{})
			Expect(err).NotTo(HaveOccurred())
			output := result.(*coding.ListFilesOutput)
			Expect(output.Entries).To(HaveLen(500))
			Expect(output.Overflow).To(BeNil())
		})

		It("reports an empty directory plainly", func() {
			runner.execFunc = func(_ context.Context, _ *v1.ExecRequest) (*v1.ExecResponse, error) {
				return &v1.ExecResponse{}, nil
			}

			result, err := tools["list_files"].Execute(ctx, &coding.ListFilesInput{Path: "empty"})
			Expect(err).NotTo(HaveOccurred())
			Expect(tools["list_files"].Render(result).Text).To(Equal("Empty directory."))
		})

		It("surfaces what find complained about", func() {
			runner.execFunc = func(_ context.Context, _ *v1.ExecRequest) (*v1.ExecResponse, error) {
				return &v1.ExecResponse{Stderr: "find: 'nope': No such file or directory\n"}, nil
			}

			result, err := tools["list_files"].Execute(ctx, &coding.ListFilesInput{Path: "nope"})
			Expect(err).NotTo(HaveOccurred())
			Expect(tools["list_files"].Render(result).Text).To(ContainSubstring("No such file or directory"))
		})
	})

	Describe("search_files", func() {
		It("runs ripgrep via the runner and returns the matches", func() {
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				Expect(req.Command).To(HavePrefix("rg "))
				Expect(req.Command).To(ContainSubstring("-e 'TODO'"))
				Expect(req.WorkingDir).To(Equal("/home/kvarn/workspace"))
				return &v1.ExecResponse{ExitCode: 0, Stdout: "main.go:10:// TODO: fix this\n"}, nil
			}

			result, err := tools["search_files"].Execute(ctx, &coding.SearchFilesInput{Pattern: "TODO"})
			Expect(err).NotTo(HaveOccurred())
			output := result.(*coding.SearchFilesOutput)
			Expect(output.Matches).To(Equal([]string{"main.go:10:// TODO: fix this"}))
			Expect(output.Overflow).To(BeNil())
			Expect(tools["search_files"].Render(result).Text).To(ContainSubstring("TODO"))
		})

		It("skips .git and searches other hidden files", func() {
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				Expect(req.Command).To(ContainSubstring("--hidden"))
				Expect(req.Command).To(ContainSubstring("--glob='!.git/'"))
				return &v1.ExecResponse{}, nil
			}

			_, err := tools["search_files"].Execute(ctx, &coding.SearchFilesInput{Pattern: "x"})
			Expect(err).NotTo(HaveOccurred())
		})

		It("includes glob filter when provided", func() {
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				Expect(req.Command).To(ContainSubstring("--glob='*.go'"))
				return &v1.ExecResponse{}, nil
			}

			_, err := tools["search_files"].Execute(ctx, &coding.SearchFilesInput{
				Pattern: "func",
				Glob:    "*.go",
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("passes a pattern and path that start with a dash as operands", func() {
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				Expect(req.Command).To(ContainSubstring("-e '--exclude' -- '-weird-dir'"))
				return &v1.ExecResponse{}, nil
			}

			_, err := tools["search_files"].Execute(ctx, &coding.SearchFilesInput{
				Pattern: "--exclude",
				Path:    "-weird-dir",
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("quotes a pattern containing shell metacharacters", func() {
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				Expect(req.Command).To(ContainSubstring(`-e 'rm -rf /; echo '\''pwned'\'''`))
				return &v1.ExecResponse{}, nil
			}

			_, err := tools["search_files"].Execute(ctx, &coding.SearchFilesInput{
				Pattern: "rm -rf /; echo 'pwned'",
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("bounds the result count and asks for one extra line to detect overflow", func() {
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				Expect(req.Command).To(HaveSuffix("| head -n 101"))
				return &v1.ExecResponse{}, nil
			}

			_, err := tools["search_files"].Execute(ctx, &coding.SearchFilesInput{Pattern: "x"})
			Expect(err).NotTo(HaveOccurred())
		})

		It("caps max_results at the ceiling", func() {
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				Expect(req.Command).To(HaveSuffix("| head -n 1001"))
				return &v1.ExecResponse{}, nil
			}

			_, err := tools["search_files"].Execute(ctx, &coding.SearchFilesInput{
				Pattern:    "x",
				MaxResults: 100000,
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("describes the size and shape of an overflowing search", func() {
			var counted bool
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				if strings.Contains(req.Command, "--count") {
					counted = true
					Expect(req.Command).NotTo(ContainSubstring("head -n"))
					return &v1.ExecResponse{Stdout: "gen/api.pb.go:600\ninternal/a.go:40\ninternal/b.go:9\ncmd/c.go:1\n"}, nil
				}
				var sb strings.Builder
				for i := range 101 {
					fmt.Fprintf(&sb, "file%d.go:1:match\n", i)
				}
				return &v1.ExecResponse{Stdout: sb.String()}, nil
			}

			result, err := tools["search_files"].Execute(ctx, &coding.SearchFilesInput{Pattern: "match"})
			Expect(err).NotTo(HaveOccurred())
			Expect(counted).To(BeTrue())

			output := result.(*coding.SearchFilesOutput)
			Expect(output.Matches).To(HaveLen(100))
			Expect(output.Overflow.TotalMatches).To(Equal(650))
			Expect(output.Overflow.TotalFiles).To(Equal(4))
			Expect(output.Overflow.TopFiles).To(HaveLen(3))
			Expect(output.Overflow.TopFiles[0].Path).To(Equal("gen/api.pb.go"))

			text := tools["search_files"].Render(result).Text
			Expect(text).To(ContainSubstring("showing 100 of 650 matches across 4 files"))
			Expect(text).To(ContainSubstring("gen/api.pb.go (600)"))
			Expect(text).To(ContainSubstring("files_only"))
		})

		It("reports overflow totals as a floor when the count pass is itself capped", func() {
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				if strings.Contains(req.Command, "--count") {
					return &v1.ExecResponse{Stdout: "a.go:5\nb.go:5\n", StdoutTotalBytes: 999999}, nil
				}
				return &v1.ExecResponse{Stdout: strings.Repeat("f.go:1:m\n", 101)}, nil
			}

			result, err := tools["search_files"].Execute(ctx, &coding.SearchFilesInput{Pattern: "m"})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.(*coding.SearchFilesOutput).Overflow.Approximate).To(BeTrue())
			Expect(tools["search_files"].Render(result).Text).To(ContainSubstring("at least"))
		})

		It("keeps the matches when the count pass fails", func() {
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				if strings.Contains(req.Command, "--count") {
					return nil, errors.New("runner is gone")
				}
				return &v1.ExecResponse{Stdout: strings.Repeat("f.go:1:m\n", 101)}, nil
			}

			result, err := tools["search_files"].Execute(ctx, &coding.SearchFilesInput{Pattern: "m"})
			Expect(err).NotTo(HaveOccurred())
			output := result.(*coding.SearchFilesOutput)
			Expect(output.Matches).To(HaveLen(100))
			Expect(output.Overflow).To(BeNil())
		})

		It("lists files when files_only is set", func() {
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				Expect(req.Command).To(ContainSubstring("--files-with-matches"))
				Expect(req.Command).NotTo(ContainSubstring("--line-number"))
				return &v1.ExecResponse{Stdout: "main.go\ninternal/a.go\n"}, nil
			}

			result, err := tools["search_files"].Execute(ctx, &coding.SearchFilesInput{
				Pattern:   "TODO",
				FilesOnly: true,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.(*coding.SearchFilesOutput).Matches).To(HaveLen(2))
			Expect(tools["search_files"].Render(result).Text).To(Equal("main.go\ninternal/a.go\n"))
		})

		It("reports no matches plainly", func() {
			runner.execFunc = func(_ context.Context, _ *v1.ExecRequest) (*v1.ExecResponse, error) {
				return &v1.ExecResponse{ExitCode: 1}, nil
			}

			result, err := tools["search_files"].Execute(ctx, &coding.SearchFilesInput{Pattern: "nothing"})
			Expect(err).NotTo(HaveOccurred())
			Expect(tools["search_files"].Render(result).Text).To(Equal("No matches."))
		})

		It("surfaces what the search complained about", func() {
			runner.execFunc = func(_ context.Context, _ *v1.ExecRequest) (*v1.ExecResponse, error) {
				return &v1.ExecResponse{Stderr: "rg: nope/: No such file or directory\n"}, nil
			}

			result, err := tools["search_files"].Execute(ctx, &coding.SearchFilesInput{
				Pattern: "x",
				Path:    "nope/",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(tools["search_files"].Render(result).Text).To(ContainSubstring("No such file or directory"))
		})
	})

	Describe("result limits", func() {
		It("asks the runner to cap the output it collects", func() {
			var execCap, sessionCap uint32
			runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
				execCap = req.MaxOutputBytes
				return &v1.ExecResponse{}, nil
			}
			runner.sessionExecFunc = func(_ context.Context, req *v1.SessionExecRequest) (*v1.SessionExecResponse, error) {
				sessionCap = req.MaxOutputBytes
				return &v1.SessionExecResponse{}, nil
			}

			_, err := tools["search_files"].Execute(ctx, &coding.SearchFilesInput{Pattern: "x"})
			Expect(err).NotTo(HaveOccurred())
			_, err = tools["exec_command"].Execute(ctx, &coding.ExecCommandInput{Command: "true"})
			Expect(err).NotTo(HaveOccurred())

			Expect(execCap).To(BeNumerically(">", 0))
			Expect(sessionCap).To(BeNumerically(">", 0))
		})

		It("clamps a result that arrives oversized anyway", func() {
			// A runner that ignores the cap stands in for anything that could
			// put more in front of the model than it asked for.
			runner.sessionExecFunc = func(_ context.Context, _ *v1.SessionExecRequest) (*v1.SessionExecResponse, error) {
				return &v1.SessionExecResponse{Stdout: strings.Repeat("chatty build line\n", 100000)}, nil
			}

			result, err := tools["exec_command"].Execute(ctx, &coding.ExecCommandInput{Command: "make"})
			Expect(err).NotTo(HaveOccurred())

			rendered := tools["exec_command"].Render(result)
			Expect(len(rendered.Text)).To(BeNumerically("<", 40*1024))
			Expect(rendered.Text).To(ContainSubstring("of this result omitted"))
			Expect(rendered.Text).To(ContainSubstring("re-run with the output narrowed"))
		})

		It("clamps read-only tools too", func() {
			// A window's worth of very long lines: bounded in lines, not bytes.
			runner.readFileFunc = func(_ context.Context, _ *v1.ReadFileRequest) (*v1.ReadFileResponse, error) {
				lines := make([]*v1.TaggedLine, 0, 2000)
				for i := range 2000 {
					lines = append(lines, &v1.TaggedLine{
						Line: int32(i + 1), Hash: "cedar", Content: strings.Repeat("x", 500),
					})
				}
				return &v1.ReadFileResponse{Version: "v1", TotalLines: 2000, Lines: lines}, nil
			}

			readOnly := make(map[string]llms.ToolDef)
			for _, t := range toolkit.ReadOnlyTools() {
				readOnly[t.Name()] = t
			}

			result, err := readOnly["read_file"].Execute(ctx, &coding.ReadFileInput{Path: "generated.go"})
			Expect(err).NotTo(HaveOccurred())

			rendered := readOnly["read_file"].Render(result)
			Expect(len(rendered.Text)).To(BeNumerically("<", 160*1024))
			Expect(rendered.Text).To(ContainSubstring("of this result omitted"))
			Expect(rendered.Text).To(ContainSubstring("start_line"))
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

	Describe("Render", func() {
		It("formats exec_command output", func() {
			result := tools["exec_command"].Render(&coding.ExecCommandOutput{
				ExitCode: 0,
				Stdout:   "ok",
			}).Text
			Expect(result).To(ContainSubstring("ok"))
			Expect(result).To(ContainSubstring("[exit code: 0]"))
		})

		It("formats edit_file success output", func() {
			result := tools["edit_file"].Render(&coding.EditFileOutput{
				Version:    "vnew",
				TotalLines: 12,
				Context: []coding.TaggedLineView{
					{Line: 3, Hash: "cedar", Content: "x"},
				},
			}).Text
			Expect(result).To(ContainSubstring("Edit applied"))
			Expect(result).To(ContainSubstring("vnew"))
			Expect(result).To(ContainSubstring("3:cedar|x"))
		})

		It("formats write_file success output", func() {
			result := tools["write_file"].Render(&coding.WriteFileOutput{Version: "vv", TotalLines: 3}).Text
			Expect(result).To(ContainSubstring("Wrote file"))
			Expect(result).To(ContainSubstring("vv"))
		})
	})
	Describe("task list management", func() {
		It("starts with an empty list", func() {
			result, err := tools["list_tasks"].Execute(ctx, &coding.ListTasksInput{})
			Expect(err).NotTo(HaveOccurred())
			Expect(tools["list_tasks"].Render(result).Text).To(ContainSubstring("Internal task list is empty."))
		})

		It("can add a task and list it", func() {
			addResult, err := tools["add_task"].Execute(ctx, &coding.AddTaskInput{
				Description: "Fix the build script",
			})
			Expect(err).NotTo(HaveOccurred())
			addStr := tools["add_task"].Render(addResult).Text
			Expect(addStr).To(ContainSubstring("Added task ID 1: \"Fix the build script\"."))
			Expect(addStr).To(ContainSubstring("- [todo] ID 1: Fix the build script"))

			// Now verify it shows up in list_tasks
			listResult, err := tools["list_tasks"].Execute(ctx, &coding.ListTasksInput{})
			Expect(err).NotTo(HaveOccurred())
			listStr := tools["list_tasks"].Render(listResult).Text
			Expect(listStr).To(ContainSubstring("- [todo] ID 1: Fix the build script"))
		})

		It("can update a task's status and description", func() {
			_, err := tools["add_task"].Execute(ctx, &coding.AddTaskInput{
				Description: "Write tests",
			})
			Expect(err).NotTo(HaveOccurred())

			newDesc := "Write robust tests"
			updateResult, err := tools["update_task"].Execute(ctx, &coding.UpdateTaskInput{
				ID:          "1",
				Status:      "in_progress",
				Description: &newDesc,
			})
			Expect(err).NotTo(HaveOccurred())
			updateStr := tools["update_task"].Render(updateResult).Text
			Expect(updateStr).To(ContainSubstring("Updated task ID 1 to status \"in_progress\"."))
			Expect(updateStr).To(ContainSubstring("- [in_progress] ID 1: Write robust tests"))

			// Mark completed
			updateResult, err = tools["update_task"].Execute(ctx, &coding.UpdateTaskInput{
				ID:     "1",
				Status: "completed",
			})
			Expect(err).NotTo(HaveOccurred())
			updateStr = tools["update_task"].Render(updateResult).Text
			Expect(updateStr).To(ContainSubstring("- [completed] ID 1: Write robust tests"))
		})

		It("returns an error when updating a non-existent task ID", func() {
			newDesc := "Do something"
			_, err := tools["update_task"].Execute(ctx, &coding.UpdateTaskInput{
				ID:          "999",
				Status:      "in_progress",
				Description: &newDesc,
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("task with ID 999 not found"))
		})

		It("returns an error when status is invalid", func() {
			_, err := tools["add_task"].Execute(ctx, &coding.AddTaskInput{
				Description: "Test invalid status",
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = tools["update_task"].Execute(ctx, &coding.UpdateTaskInput{
				ID:     "1",
				Status: "invalid-status-value",
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid status"))
		})
	})
})
