package run

import (
	"bytes"
	"context"
	stderrors "errors"
	"io"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/sandbox"
	"github.com/aholstenson/kvarn/internal/taskui"
)

// stubRunner is a minimal sandbox.RunnerProxy that returns pre-canned
// responses keyed by argv[0]. Other surface area returns ErrUnsupported.
type stubRunner struct {
	execResponses map[string]*v1.ExecResponse
	execCalls     []*v1.ExecRequest
}

func (s *stubRunner) Exec(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
	s.execCalls = append(s.execCalls, req)
	if resp, ok := s.execResponses[strings.Join(append([]string{req.Command}, req.Args...), " ")]; ok {
		return resp, nil
	}
	return &v1.ExecResponse{ExitCode: 0}, nil
}

func (s *stubRunner) CreateSession(_ context.Context, _ *v1.CreateSessionRequest) (*v1.CreateSessionResponse, error) {
	return nil, stderrors.ErrUnsupported
}
func (s *stubRunner) SessionExec(_ context.Context, _ *v1.SessionExecRequest, _ sandbox.OutputCallback) (*v1.SessionExecResponse, error) {
	return nil, stderrors.ErrUnsupported
}
func (s *stubRunner) CloseSession(_ context.Context, _ *v1.CloseSessionRequest) (*v1.CloseSessionResponse, error) {
	return nil, stderrors.ErrUnsupported
}
func (s *stubRunner) UploadFiles(_ context.Context, _ *v1.UploadFilesRequest) (*v1.UploadFilesResponse, error) {
	return nil, stderrors.ErrUnsupported
}
func (s *stubRunner) ReadFile(_ context.Context, _ *v1.ReadFileRequest) (*v1.ReadFileResponse, error) {
	return nil, stderrors.ErrUnsupported
}
func (s *stubRunner) EditFile(_ context.Context, _ *v1.EditFileRequest) (*v1.EditFileResponse, error) {
	return nil, stderrors.ErrUnsupported
}
func (s *stubRunner) WriteFile(_ context.Context, _ *v1.WriteFileRequest) (*v1.WriteFileResponse, error) {
	return nil, stderrors.ErrUnsupported
}
func (s *stubRunner) StreamToGuest(_ context.Context, _ string, _ io.Reader, _ int64) error {
	return stderrors.ErrUnsupported
}
func (s *stubRunner) StreamFromGuest(_ context.Context, _ string, _ io.Writer) error {
	return stderrors.ErrUnsupported
}

// stubSandbox satisfies the extractor interface for emitApply tests.
type stubSandbox struct {
	runner            sandbox.RunnerProxy
	workdir           string
	extractCalls      int
	extractDest       string
	extractReturnsErr error
}

func (s *stubSandbox) GetRunner() sandbox.RunnerProxy { return s.runner }
func (s *stubSandbox) GetWorkingDir() string          { return s.workdir }
func (s *stubSandbox) ExtractChanges(_ context.Context, destDir string) error {
	s.extractCalls++
	s.extractDest = destDir
	return s.extractReturnsErr
}

var _ = Describe("emitDiff", func() {
	It("writes git diff stdout and returns its line count", func() {
		runner := &stubRunner{
			execResponses: map[string]*v1.ExecResponse{
				"git diff HEAD": {Stdout: "diff --git a/x b/x\n@@\n-foo\n+bar\n"},
			},
		}
		var out bytes.Buffer
		lines, err := emitDiff(context.Background(), runner, "/work", &out)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.String()).To(ContainSubstring("diff --git a/x b/x"))
		Expect(lines).To(Equal(4))
	})

	It("returns 0 lines for an empty diff", func() {
		runner := &stubRunner{
			execResponses: map[string]*v1.ExecResponse{
				"git diff HEAD": {Stdout: ""},
			},
		}
		var out bytes.Buffer
		lines, err := emitDiff(context.Background(), runner, "/work", &out)
		Expect(err).NotTo(HaveOccurred())
		Expect(lines).To(Equal(0))
		Expect(out.String()).To(BeEmpty())
	})
})

var _ = Describe("emitApply", func() {
	It("classifies changes, calls ExtractChanges, and reports counts", func() {
		runner := &stubRunner{
			execResponses: map[string]*v1.ExecResponse{
				"git add -A": {},
				"git diff --cached --name-status HEAD": {
					Stdout: "A\tnew.txt\nM\tchanged.go\nD\tgone.md\n",
				},
			},
		}
		sess := &stubSandbox{runner: runner, workdir: "/work"}

		var out bytes.Buffer
		count, err := emitApply(context.Background(), sess, "/host/dir", &out)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(2)) // 1 added + 1 modified
		Expect(sess.extractCalls).To(Equal(1))
		Expect(sess.extractDest).To(Equal("/host/dir"))
		Expect(out.String()).To(ContainSubstring("Applied 2 files (added 1, modified 1, removed 1)"))
	})

	It("propagates an ExtractChanges error", func() {
		runner := &stubRunner{
			execResponses: map[string]*v1.ExecResponse{
				"git add -A":                           {},
				"git diff --cached --name-status HEAD": {},
			},
		}
		sess := &stubSandbox{
			runner:            runner,
			workdir:           "/work",
			extractReturnsErr: stderrors.New("disk full"),
		}

		_, err := emitApply(context.Background(), sess, "/host/dir", io.Discard)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("disk full"))
	})
})

var _ = Describe("classifyChanges", func() {
	It("counts adds, modifies, and deletes from --name-status output", func() {
		runner := &stubRunner{
			execResponses: map[string]*v1.ExecResponse{
				"git add -A": {},
				"git diff --cached --name-status HEAD": {
					Stdout: "A\ta\nA\tb\nM\tc\nR100\told\tnew\nD\tx\n",
				},
			},
		}
		added, modified, deleted, err := classifyChanges(context.Background(), runner, "/work")
		Expect(err).NotTo(HaveOccurred())
		Expect(added).To(Equal(2))
		Expect(modified).To(Equal(2)) // M + R both count as modified
		Expect(deleted).To(Equal(1))
	})

	It("returns an error when git add fails", func() {
		runner := &stubRunner{
			execResponses: map[string]*v1.ExecResponse{
				"git add -A": {ExitCode: 1, Stderr: "permission denied"},
			},
		}
		_, _, _, err := classifyChanges(context.Background(), runner, "/work")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("permission denied"))
	})
})

var _ = Describe("summaryState.finish", func() {
	It("emits agent-ok and diff-line count when nothing failed", func() {
		s := &summaryState{passed: 3}
		var out bytes.Buffer
		err := s.finish(&out, nil, "diff", 12, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.String()).To(ContainSubstring("agent: ok"))
		Expect(out.String()).To(ContainSubstring("diff: 12 lines"))
		Expect(out.String()).To(ContainSubstring("3 passed"))
	})

	It("emits agent-failed and returns an error when the agent failed", func() {
		s := &summaryState{agentFailed: true, passed: 1}
		var out bytes.Buffer
		err := s.finish(&out, nil, "apply", 0, 2)
		Expect(err).To(HaveOccurred())
		Expect(out.String()).To(ContainSubstring("agent: failed"))
		Expect(out.String()).To(ContainSubstring("applied 2 files"))
	})

	It("returns an error when required validation failed", func() {
		s := &summaryState{requiredFailed: true, failed: 1}
		err := s.finish(io.Discard, nil, "diff", 0, 0)
		Expect(err).To(HaveOccurred())
	})

	It("propagates an explicit error and still prints the summary", func() {
		s := &summaryState{}
		var out bytes.Buffer
		err := s.finish(&out, stderrors.New("boom"), "diff", 0, 0)
		Expect(err).To(MatchError("boom"))
		Expect(out.String()).To(ContainSubstring("agent: ok"))
	})
})

var _ = Describe("makeProgressCallback", func() {
	It("updates the parent suffix on ToolUse and logs a check on ToolResult", func() {
		renderer := taskui.New(io.Discard, false)
		parent := renderer.AddItem("Agent")
		var count int
		cb := makeProgressCallback(renderer, parent, &count)

		cb(agent.ProgressToolUse{ToolID: "read_file", ArgumentsJSON: `{"path":"x"}`})
		Expect(count).To(Equal(1))
		Expect(parent.Children).To(BeEmpty())
		Expect(parent.Suffix).To(ContainSubstring("read_file"))
		Expect(parent.Suffix).To(ContainSubstring("1 tools"))

		cb(agent.ProgressToolResult{ToolID: "read_file", Result: "ok"})
		Expect(parent.Output).To(ContainElement(ContainSubstring("✓ read_file")))
		Expect(parent.Output[len(parent.Output)-1]).To(ContainSubstring(`{"path":"x"}`))
	})

	It("pairs overlapping calls to the same tool via FIFO order", func() {
		renderer := taskui.New(io.Discard, false)
		parent := renderer.AddItem("Agent")
		var count int
		cb := makeProgressCallback(renderer, parent, &count)

		cb(agent.ProgressToolUse{ToolID: "read_file", ArgumentsJSON: `{"path":"a"}`})
		cb(agent.ProgressToolUse{ToolID: "read_file", ArgumentsJSON: `{"path":"b"}`})
		cb(agent.ProgressToolResult{ToolID: "read_file"})
		cb(agent.ProgressToolResult{ToolID: "read_file"})

		Expect(count).To(Equal(2))
		Expect(parent.Output).To(HaveLen(2))
		Expect(parent.Output[0]).To(ContainSubstring(`{"path":"a"}`))
		Expect(parent.Output[1]).To(ContainSubstring(`{"path":"b"}`))
	})

	It("marks the result line with ✗ and the first error line on IsError", func() {
		renderer := taskui.New(io.Discard, false)
		parent := renderer.AddItem("Agent")
		var count int
		cb := makeProgressCallback(renderer, parent, &count)

		cb(agent.ProgressToolUse{ToolID: "edit_file", ArgumentsJSON: "{}"})
		cb(agent.ProgressToolResult{ToolID: "edit_file", Result: "no such file\n(more details)", IsError: true})

		Expect(parent.Output).To(HaveLen(1))
		Expect(parent.Output[0]).To(HavePrefix("✗ edit_file"))
		Expect(parent.Output[0]).To(ContainSubstring("no such file"))
		Expect(parent.Output[0]).NotTo(ContainSubstring("(more details)"))
	})

	It("appends text messages as output under the parent", func() {
		renderer := taskui.New(io.Discard, false)
		parent := renderer.AddItem("Agent")
		var count int
		cb := makeProgressCallback(renderer, parent, &count)

		cb(agent.ProgressTextMessage{Text: "Thinking...\nNext step"})
		Expect(parent.Output).To(ContainElement(ContainSubstring("Thinking...")))
		Expect(parent.Output).To(ContainElement(ContainSubstring("Next step")))
	})
})

var _ = Describe("shortArgs", func() {
	It("collapses newlines and truncates long blobs", func() {
		long := strings.Repeat("x", 200)
		short := shortArgs(long)
		Expect(short).To(HaveSuffix("…"))
		Expect(strings.Count(short, "x")).To(Equal(79))
	})

	It("leaves short single-line input alone", func() {
		Expect(shortArgs(`{"a":1}`)).To(Equal(`{"a":1}`))
	})
})
