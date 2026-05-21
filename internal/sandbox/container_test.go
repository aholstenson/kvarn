package sandbox_test

import (
	"context"
	"fmt"
	"io"
	"strings"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// mockProxy records all calls and returns configurable responses.
type mockProxy struct {
	execCalls          []*v1.ExecRequest
	createSessionCalls []*v1.CreateSessionRequest
	sessionExecCalls   []*v1.SessionExecRequest
	closeSessionCalls  []*v1.CloseSessionRequest
	uploadCalls        []*v1.UploadFilesRequest
	readFileCalls      []*v1.ReadFileRequest
	editFileCalls      []*v1.EditFileRequest
	writeFileCalls     []*v1.WriteFileRequest
	execResponses      []*v1.ExecResponse
	execErrors         []error
	execCallCounter    int
}

func newMockProxy() *mockProxy {
	return &mockProxy{}
}

func (m *mockProxy) pushExecResponse(resp *v1.ExecResponse, err error) {
	m.execResponses = append(m.execResponses, resp)
	m.execErrors = append(m.execErrors, err)
}

func (m *mockProxy) Exec(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
	m.execCalls = append(m.execCalls, req)
	idx := m.execCallCounter
	m.execCallCounter++
	if idx < len(m.execResponses) {
		return m.execResponses[idx], m.execErrors[idx]
	}
	return &v1.ExecResponse{ExitCode: 0}, nil
}

func (m *mockProxy) CreateSession(_ context.Context, req *v1.CreateSessionRequest) (*v1.CreateSessionResponse, error) {
	m.createSessionCalls = append(m.createSessionCalls, req)
	return &v1.CreateSessionResponse{SessionId: "mock-session"}, nil
}

func (m *mockProxy) SessionExec(_ context.Context, req *v1.SessionExecRequest, _ sandbox.OutputCallback) (*v1.SessionExecResponse, error) {
	m.sessionExecCalls = append(m.sessionExecCalls, req)
	return &v1.SessionExecResponse{ExitCode: 0}, nil
}

func (m *mockProxy) CloseSession(_ context.Context, req *v1.CloseSessionRequest) (*v1.CloseSessionResponse, error) {
	m.closeSessionCalls = append(m.closeSessionCalls, req)
	return &v1.CloseSessionResponse{}, nil
}

func (m *mockProxy) UploadFiles(_ context.Context, req *v1.UploadFilesRequest) (*v1.UploadFilesResponse, error) {
	m.uploadCalls = append(m.uploadCalls, req)
	return &v1.UploadFilesResponse{}, nil
}

func (m *mockProxy) ReadFile(_ context.Context, req *v1.ReadFileRequest) (*v1.ReadFileResponse, error) {
	m.readFileCalls = append(m.readFileCalls, req)
	return &v1.ReadFileResponse{}, nil
}

func (m *mockProxy) EditFile(_ context.Context, req *v1.EditFileRequest) (*v1.EditFileResponse, error) {
	m.editFileCalls = append(m.editFileCalls, req)
	return &v1.EditFileResponse{}, nil
}

func (m *mockProxy) WriteFile(_ context.Context, req *v1.WriteFileRequest) (*v1.WriteFileResponse, error) {
	m.writeFileCalls = append(m.writeFileCalls, req)
	return &v1.WriteFileResponse{}, nil
}

func (m *mockProxy) StreamToGuest(_ context.Context, _ string, _ io.Reader, _ int64) error {
	return nil
}

func (m *mockProxy) StreamFromGuest(_ context.Context, _ string, _ io.Writer) error {
	return nil
}

var _ = Describe("ContainerProxy", func() {
	var (
		inner *mockProxy
		proxy *sandbox.ContainerProxy
		ctx   context.Context
	)

	BeforeEach(func() {
		inner = newMockProxy()
		proxy = sandbox.NewContainerProxy(inner, "kvarn-workspace")
		ctx = context.Background()
	})

	Describe("Start", func() {
		It("sends podman run with workspace mount and network host", func() {
			err := proxy.Start(ctx, "node:20", "/home/kvarn/workspace")
			Expect(err).NotTo(HaveOccurred())
			Expect(inner.execCalls).To(HaveLen(1))

			call := inner.execCalls[0]
			Expect(call.Command).To(Equal("podman"))
			args := strings.Join(call.Args, " ")
			Expect(args).To(ContainSubstring("run -d"))
			Expect(args).To(ContainSubstring("--name kvarn-workspace"))
			Expect(args).To(ContainSubstring("-v /home/kvarn/workspace:/home/kvarn/workspace"))
			Expect(args).To(ContainSubstring("--network host"))
			Expect(args).To(ContainSubstring("node:20"))
			Expect(args).To(ContainSubstring("tail -f /dev/null"))
		})

		It("returns error when podman run fails", func() {
			inner.pushExecResponse(&v1.ExecResponse{ExitCode: 1, Stderr: "image not found"}, nil)
			err := proxy.Start(ctx, "node:20", "/home/kvarn/workspace")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("start container failed"))
		})

		It("returns error when exec itself fails", func() {
			inner.pushExecResponse(nil, fmt.Errorf("connection lost"))
			err := proxy.Start(ctx, "node:20", "/home/kvarn/workspace")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("start container"))
		})
	})

	Describe("Stop", func() {
		It("sends podman rm -f", func() {
			proxy.Stop(ctx)
			Expect(inner.execCalls).To(HaveLen(1))
			call := inner.execCalls[0]
			Expect(call.Command).To(Equal("podman"))
			Expect(call.Args).To(Equal([]string{"rm", "-f", "kvarn-workspace"}))
		})
	})

	Describe("Exec", func() {
		It("rewrites command with explicit args to podman exec", func() {
			resp, err := proxy.Exec(ctx, &v1.ExecRequest{
				Command:    "npm",
				Args:       []string{"install"},
				WorkingDir: "/home/kvarn/workspace",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.ExitCode).To(Equal(int32(0)))

			Expect(inner.execCalls).To(HaveLen(1))
			call := inner.execCalls[0]
			Expect(call.Command).To(Equal("podman"))
			Expect(call.Args).To(Equal([]string{
				"exec", "-w", "/home/kvarn/workspace", "kvarn-workspace", "npm", "install",
			}))
			Expect(call.WorkingDir).To(Equal("/"))
		})

		It("rewrites command with no args to podman exec sh -c", func() {
			resp, err := proxy.Exec(ctx, &v1.ExecRequest{
				Command:    "npm install && npm run build",
				WorkingDir: "/home/kvarn/workspace",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.ExitCode).To(Equal(int32(0)))

			Expect(inner.execCalls).To(HaveLen(1))
			call := inner.execCalls[0]
			Expect(call.Command).To(Equal("podman"))
			Expect(call.Args).To(Equal([]string{
				"exec", "-w", "/home/kvarn/workspace", "kvarn-workspace", "sh", "-c", "npm install && npm run build",
			}))
		})

		It("defaults working dir to / when not set", func() {
			_, err := proxy.Exec(ctx, &v1.ExecRequest{
				Command: "whoami",
			})
			Expect(err).NotTo(HaveOccurred())
			call := inner.execCalls[0]
			Expect(call.Args[1]).To(Equal("-w"))
			Expect(call.Args[2]).To(Equal("/"))
		})
	})

	Describe("CreateSession", func() {
		It("sets container field on inner call", func() {
			resp, err := proxy.CreateSession(ctx, &v1.CreateSessionRequest{
				WorkingDir: "/home/kvarn/workspace",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.SessionId).To(Equal("mock-session"))

			Expect(inner.createSessionCalls).To(HaveLen(1))
			call := inner.createSessionCalls[0]
			Expect(call.WorkingDir).To(Equal("/home/kvarn/workspace"))
			Expect(call.Container).To(Equal("kvarn-workspace"))
		})
	})

	Describe("SessionExec", func() {
		It("passes through to inner", func() {
			req := &v1.SessionExecRequest{SessionId: "sess-1", Command: "echo hello"}
			_, err := proxy.SessionExec(ctx, req, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(inner.sessionExecCalls).To(HaveLen(1))
			Expect(inner.sessionExecCalls[0]).To(Equal(req))
		})
	})

	Describe("CloseSession", func() {
		It("passes through to inner", func() {
			req := &v1.CloseSessionRequest{SessionId: "sess-1"}
			_, err := proxy.CloseSession(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(inner.closeSessionCalls).To(HaveLen(1))
			Expect(inner.closeSessionCalls[0]).To(Equal(req))
		})
	})

	Describe("Passthrough methods", func() {
		It("passes UploadFiles through to inner", func() {
			req := &v1.UploadFilesRequest{WorkingDir: "/home/kvarn/workspace"}
			_, err := proxy.UploadFiles(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(inner.uploadCalls).To(HaveLen(1))
			Expect(inner.uploadCalls[0]).To(Equal(req))
		})

		It("passes ReadFile through to inner", func() {
			req := &v1.ReadFileRequest{Path: "/home/kvarn/workspace/main.go"}
			_, err := proxy.ReadFile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(inner.readFileCalls).To(HaveLen(1))
			Expect(inner.readFileCalls[0]).To(Equal(req))
		})

		It("passes EditFile through to inner", func() {
			req := &v1.EditFileRequest{Path: "/home/kvarn/workspace/main.go"}
			_, err := proxy.EditFile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(inner.editFileCalls).To(HaveLen(1))
			Expect(inner.editFileCalls[0]).To(Equal(req))
		})

	})
})
