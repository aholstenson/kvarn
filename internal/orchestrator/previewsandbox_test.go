package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/sandbox"
)

// PreviewSandbox carries two unrelated jobs: dialling into the guest, which
// most specs care about, and the handful of methods a state capture needs,
// which most specs do not. statelessSandbox supplies the second half as "this
// VM cannot hold state", so a spec about routing does not have to describe one.
type statelessSandbox struct{}

func (statelessSandbox) BareRunner() sandbox.RunnerProxy  { return nil }
func (statelessSandbox) GetRunner() sandbox.RunnerProxy   { return nil }
func (statelessSandbox) GetShellSessionID() string        { return "" }
func (statelessSandbox) Processes() sandbox.ProcessRunner { return nil }

// guestRecorder is a RunnerProxy and ProcessRunner that answers the scripts a
// capture runs and records what crossed it, so a spec can assert on the
// sequence — quiesce, stop the servers, tar — without a VM.
type guestRecorder struct {
	mu sync.Mutex
	// scripts are the privileged shell scripts run in the guest, in order.
	scripts []string
	// shellCommands are what ran in the boot's shell session: the save and
	// restore hooks.
	shellCommands []string
	// stopped are the process IDs StopProcess was called with.
	stopped []string
	// order interleaves everything, so "the servers stopped before the tar ran"
	// is expressible.
	order []string

	// hasState decides what the state probe answers.
	hasState bool
	// tarFails makes the archiving script fail.
	tarFails bool
	// archive is what StreamFromGuest hands back.
	archive []byte
	// restored is what StreamToGuest received.
	restored []byte
	// shellExit, when non-zero, fails every shell command.
	shellExit int32
}

func newGuestRecorder() *guestRecorder {
	return &guestRecorder{archive: []byte("tar-bytes")}
}

func (g *guestRecorder) record(kind, what string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.order = append(g.order, kind+":"+what)
}

func (g *guestRecorder) Exec(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
	script := strings.Join(req.Args, " ")

	g.mu.Lock()
	g.scripts = append(g.scripts, script)
	g.mu.Unlock()

	switch {
	case req.Command == "rm":
		return &v1.ExecResponse{}, nil
	case strings.Contains(script, "tar -C / --zstd -cf"):
		g.record("guest", "tar")
		if g.tarFails {
			return &v1.ExecResponse{ExitCode: 2, Stderr: "tar: broken"}, nil
		}
		return &v1.ExecResponse{}, nil
	case strings.Contains(script, "tar -C / --zstd -xf"):
		g.record("guest", "untar")
		return &v1.ExecResponse{}, nil
	case strings.Contains(script, "ls -A"):
		g.record("guest", "probe")
		if g.hasState {
			return &v1.ExecResponse{}, nil
		}
		return &v1.ExecResponse{ExitCode: 1}, nil
	default:
		g.record("guest", "sh")
		return &v1.ExecResponse{}, nil
	}
}

func (g *guestRecorder) SessionExec(_ context.Context, req *v1.SessionExecRequest, _ sandbox.OutputCallback) (*v1.SessionExecResponse, error) {
	g.mu.Lock()
	g.shellCommands = append(g.shellCommands, req.Command)
	g.mu.Unlock()
	g.record("shell", req.Command)
	return &v1.SessionExecResponse{ExitCode: g.shellExit}, nil
}

func (g *guestRecorder) StreamToGuest(_ context.Context, _ string, src io.Reader, _ int64) error {
	data, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.restored = data
	g.mu.Unlock()
	g.record("guest", "stream-in")
	return nil
}

func (g *guestRecorder) StreamFromGuest(_ context.Context, _ string, dest io.Writer) error {
	g.record("guest", "stream-out")
	_, err := dest.Write(g.archive)
	return err
}

func (g *guestRecorder) StartProcess(context.Context, *v1.StartProcessRequest, sandbox.OutputCallback, sandbox.ProcessExitCallback) (*v1.StartProcessResponse, error) {
	return nil, errors.ErrUnsupported
}

func (g *guestRecorder) StopProcess(_ context.Context, req *v1.StopProcessRequest) (*v1.StopProcessResponse, error) {
	g.mu.Lock()
	g.stopped = append(g.stopped, req.ProcessId)
	g.mu.Unlock()
	g.record("stop", req.ProcessId)
	return &v1.StopProcessResponse{}, nil
}

func (g *guestRecorder) ListProcesses(context.Context, *v1.ListProcessesRequest) (*v1.ListProcessesResponse, error) {
	return nil, errors.ErrUnsupported
}

// The remaining RunnerProxy methods have no part in a capture.
func (g *guestRecorder) CreateSession(context.Context, *v1.CreateSessionRequest) (*v1.CreateSessionResponse, error) {
	return nil, errors.ErrUnsupported
}
func (g *guestRecorder) CloseSession(context.Context, *v1.CloseSessionRequest) (*v1.CloseSessionResponse, error) {
	return nil, errors.ErrUnsupported
}
func (g *guestRecorder) UploadFiles(context.Context, *v1.UploadFilesRequest) (*v1.UploadFilesResponse, error) {
	return nil, errors.ErrUnsupported
}
func (g *guestRecorder) ReadFile(context.Context, *v1.ReadFileRequest) (*v1.ReadFileResponse, error) {
	return nil, errors.ErrUnsupported
}
func (g *guestRecorder) EditFile(context.Context, *v1.EditFileRequest) (*v1.EditFileResponse, error) {
	return nil, errors.ErrUnsupported
}
func (g *guestRecorder) WriteFile(context.Context, *v1.WriteFileRequest) (*v1.WriteFileResponse, error) {
	return nil, errors.ErrUnsupported
}

func (g *guestRecorder) events() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string{}, g.order...)
}

func (g *guestRecorder) stoppedProcesses() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string{}, g.stopped...)
}

func (g *guestRecorder) ranTar() bool {
	for _, e := range g.events() {
		if e == "guest:tar" {
			return true
		}
	}
	return false
}

func (g *guestRecorder) String() string { return fmt.Sprint(g.events()) }
