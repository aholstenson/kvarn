package sandbox_test

import (
	"context"
	"io"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/sandbox"
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
	uploadError        error
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
	if m.uploadError != nil {
		return nil, m.uploadError
	}
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
