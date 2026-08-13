package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/dispatch"
)

// OutputCallback is called with new stdout/stderr content as it becomes available
// during command execution.
type OutputCallback func(stdout, stderr string)

// RunnerProxy abstracts the bridge channel mechanism for sending commands to the VM runner.
type RunnerProxy interface {
	Exec(ctx context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error)
	CreateSession(ctx context.Context, req *v1.CreateSessionRequest) (*v1.CreateSessionResponse, error)
	SessionExec(ctx context.Context, req *v1.SessionExecRequest, onOutput OutputCallback) (*v1.SessionExecResponse, error)
	CloseSession(ctx context.Context, req *v1.CloseSessionRequest) (*v1.CloseSessionResponse, error)
	UploadFiles(ctx context.Context, req *v1.UploadFilesRequest) (*v1.UploadFilesResponse, error)
	ReadFile(ctx context.Context, req *v1.ReadFileRequest) (*v1.ReadFileResponse, error)
	EditFile(ctx context.Context, req *v1.EditFileRequest) (*v1.EditFileResponse, error)
	WriteFile(ctx context.Context, req *v1.WriteFileRequest) (*v1.WriteFileResponse, error)
	StreamToGuest(ctx context.Context, destPath string, src io.Reader, size int64) error
	StreamFromGuest(ctx context.Context, srcPath string, dest io.Writer) error
}

// ProcessExitCallback is invoked when a long-lived process started through
// StartProcess ends, whether it was stopped or died on its own.
type ProcessExitCallback func(exitCode int32, err error)

// ProcessRunner manages processes in the guest that outlive the call starting
// them — the servers a preview environment runs. It is separate from
// RunnerProxy because only the bridge-backed proxy can support it: the
// callbacks are fed by the runner's unsolicited reports, which have nowhere to
// arrive on a proxy that is not attached to a live bridge.
type ProcessRunner interface {
	// StartProcess spawns the process and returns once it is running. Output
	// arrives on onOutput until it ends; onExit fires exactly once when it
	// does. The caller supplies req.ProcessId.
	StartProcess(ctx context.Context, req *v1.StartProcessRequest, onOutput OutputCallback, onExit ProcessExitCallback) (*v1.StartProcessResponse, error)
	StopProcess(ctx context.Context, req *v1.StopProcessRequest) (*v1.StopProcessResponse, error)
	ListProcesses(ctx context.Context, req *v1.ListProcessesRequest) (*v1.ListProcessesResponse, error)
}

// commandWaiter is registered per in-flight command so the dispatcher can
// route results and output chunks to the correct caller.
type commandWaiter struct {
	resultCh chan *v1.CommandResult
	outputCh chan *v1.OutputChunk // nil for non-SessionExec commands
}

// BridgeProxy implements RunnerProxy by sending commands through the bridge
// channel mechanism (commandCh/resultCh) used by the orchestrator.
type BridgeProxy struct {
	commandCh chan<- *v1.RunnerCommand
	runner    *dispatch.PendingRunner
	nextID    atomic.Int64

	mu      sync.Mutex
	waiters map[string]*commandWaiter
	// procs holds the callbacks for long-lived processes, keyed on process ID.
	// Output for one arrives on the same channel as command output but is not
	// tied to any in-flight command, so it is routed from here instead.
	procs map[string]*processSub
}

// processSub is the caller's interest in one long-lived process.
type processSub struct {
	onOutput OutputCallback
	onExit   ProcessExitCallback
	exitOnce sync.Once
}

// NewBridgeProxy creates a RunnerProxy backed by the given command/result/output channels.
// It starts background goroutines to dispatch results and output to per-command waiters.
func NewBridgeProxy(commandCh chan<- *v1.RunnerCommand, resultCh <-chan *v1.CommandResult, outputCh <-chan *v1.OutputChunk, runner *dispatch.PendingRunner) *BridgeProxy {
	p := &BridgeProxy{
		commandCh: commandCh,
		runner:    runner,
		waiters:   make(map[string]*commandWaiter),
		procs:     make(map[string]*processSub),
	}
	go p.dispatchResults(resultCh)
	go p.dispatchOutput(outputCh)
	if runner != nil {
		go p.dispatchProcessEvents(runner.ProcessEventCh)
	}
	return p
}

// dispatchResults reads results from the shared channel and routes each to the
// waiter registered for that command ID.
func (p *BridgeProxy) dispatchResults(resultCh <-chan *v1.CommandResult) {
	for result := range resultCh {
		p.mu.Lock()
		w, ok := p.waiters[result.CommandId]
		p.mu.Unlock()

		if !ok {
			slog.Warn("received result for unknown command, discarding",
				"command_id", result.CommandId)
			continue
		}

		w.resultCh <- result
	}
}

// dispatchOutput reads output chunks from the shared channel and routes each
// to the waiter registered for that command ID.
func (p *BridgeProxy) dispatchOutput(outputCh <-chan *v1.OutputChunk) {
	for chunk := range outputCh {
		p.mu.Lock()
		w, ok := p.waiters[chunk.CommandId]
		sub := p.procs[chunk.CommandId]
		p.mu.Unlock()

		// A long-lived process keys its output on its process ID, which is not
		// a command anyone is waiting on. Its callback runs here rather than
		// through a channel because there is no command loop to drain one;
		// callbacks must therefore not block.
		if sub != nil {
			if sub.onOutput != nil {
				sub.onOutput(chunk.Stdout, chunk.Stderr)
			}
			continue
		}

		if !ok || w.outputCh == nil {
			continue
		}

		select {
		case w.outputCh <- chunk:
		default:
			slog.Warn("output channel full, dropping chunk",
				"command_id", chunk.CommandId)
		}
	}
}

// dispatchProcessEvents routes unsolicited process notifications to the
// subscriber that started the process.
func (p *BridgeProxy) dispatchProcessEvents(eventCh <-chan *v1.ProcessEvent) {
	for event := range eventCh {
		if event.Kind != v1.ProcessEventKind_PROCESS_EVENT_KIND_EXITED {
			continue
		}

		p.mu.Lock()
		sub := p.procs[event.ProcessId]
		delete(p.procs, event.ProcessId)
		p.mu.Unlock()

		if sub == nil {
			slog.Debug("process exit for unknown process, discarding", "process_id", event.ProcessId)
			continue
		}
		p.notifyExit(sub, event.ExitCode, event.Error)
	}
}

// notifyExit delivers the exit callback at most once, so a process that both
// reports an exit and is later stopped does not fire it twice.
func (p *BridgeProxy) notifyExit(sub *processSub, exitCode int32, errMsg string) {
	sub.exitOnce.Do(func() {
		if sub.onExit == nil {
			return
		}
		var err error
		if errMsg != "" {
			err = errors.New(errMsg)
		}
		sub.onExit(exitCode, err)
	})
}

// StartProcess launches a process in the guest that outlives this call.
func (p *BridgeProxy) StartProcess(ctx context.Context, req *v1.StartProcessRequest, onOutput OutputCallback, onExit ProcessExitCallback) (*v1.StartProcessResponse, error) {
	if req.ProcessId == "" {
		return nil, errors.New("process ID is required")
	}

	// Subscribe before sending: the guest can produce output the moment the
	// process is spawned, which is before the start command's own result gets
	// back here.
	sub := &processSub{onOutput: onOutput, onExit: onExit}
	p.mu.Lock()
	if _, exists := p.procs[req.ProcessId]; exists {
		p.mu.Unlock()
		return nil, fmt.Errorf("process %q is already running", req.ProcessId)
	}
	p.procs[req.ProcessId] = sub
	p.mu.Unlock()

	result, err := p.sendAndWait(ctx, &v1.RunnerCommand{
		CommandId: p.nextCommandID(),
		Command:   &v1.RunnerCommand_StartProcess{StartProcess: req},
	})
	if err != nil {
		p.mu.Lock()
		delete(p.procs, req.ProcessId)
		p.mu.Unlock()
		return nil, err
	}
	resp := result.GetStartProcess()
	if resp == nil {
		p.mu.Lock()
		delete(p.procs, req.ProcessId)
		p.mu.Unlock()
		return nil, errors.New("unexpected result type")
	}
	return resp, nil
}

// StopProcess terminates a process in the guest and drops its subscription.
func (p *BridgeProxy) StopProcess(ctx context.Context, req *v1.StopProcessRequest) (*v1.StopProcessResponse, error) {
	result, err := p.sendAndWait(ctx, &v1.RunnerCommand{
		CommandId: p.nextCommandID(),
		Command:   &v1.RunnerCommand_StopProcess{StopProcess: req},
	})

	// Drop the subscription either way. A stop that failed because the runner
	// no longer knows the process still means nothing more will arrive for it.
	p.mu.Lock()
	sub := p.procs[req.ProcessId]
	delete(p.procs, req.ProcessId)
	p.mu.Unlock()

	if err != nil {
		return nil, err
	}
	resp := result.GetStopProcess()
	if resp == nil {
		return nil, errors.New("unexpected result type")
	}
	// The runner reaps the process as part of stopping it, so the exit event
	// may never make it across; deliver it from here so the caller always
	// learns the process is gone exactly once.
	if sub != nil {
		p.notifyExit(sub, resp.ExitCode, "")
	}
	return resp, nil
}

// ListProcesses reports the processes the guest's runner is supervising.
func (p *BridgeProxy) ListProcesses(ctx context.Context, req *v1.ListProcessesRequest) (*v1.ListProcessesResponse, error) {
	result, err := p.sendAndWait(ctx, &v1.RunnerCommand{
		CommandId: p.nextCommandID(),
		Command:   &v1.RunnerCommand_ListProcesses{ListProcesses: req},
	})
	if err != nil {
		return nil, err
	}
	resp := result.GetListProcesses()
	if resp == nil {
		return nil, errors.New("unexpected result type")
	}
	return resp, nil
}

// registerWaiter creates a per-command waiter and stores it in the map.
func (p *BridgeProxy) registerWaiter(commandID string, wantOutput bool) *commandWaiter {
	w := &commandWaiter{
		resultCh: make(chan *v1.CommandResult, 1),
	}
	if wantOutput {
		w.outputCh = make(chan *v1.OutputChunk, 64)
	}
	p.mu.Lock()
	p.waiters[commandID] = w
	p.mu.Unlock()
	return w
}

// removeWaiter removes the waiter for the given command ID.
func (p *BridgeProxy) removeWaiter(commandID string) {
	p.mu.Lock()
	delete(p.waiters, commandID)
	p.mu.Unlock()
}

func (p *BridgeProxy) nextCommandID() string {
	return fmt.Sprintf("cmd-%d", p.nextID.Add(1))
}

func (p *BridgeProxy) sendAndWait(ctx context.Context, cmd *v1.RunnerCommand) (*v1.CommandResult, error) {
	w := p.registerWaiter(cmd.CommandId, false)
	defer p.removeWaiter(cmd.CommandId)

	select {
	case p.commandCh <- cmd:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	slog.Debug("command sent, waiting for result", "command_id", cmd.CommandId)

	select {
	case result := <-w.resultCh:
		if result.Error != "" {
			return nil, fmt.Errorf("runner error: %s", result.Error)
		}
		return result, nil
	case <-ctx.Done():
		slog.Warn("command timed out waiting for result", "command_id", cmd.CommandId)
		return nil, ctx.Err()
	}
}

func (p *BridgeProxy) CreateSession(ctx context.Context, req *v1.CreateSessionRequest) (*v1.CreateSessionResponse, error) {
	result, err := p.sendAndWait(ctx, &v1.RunnerCommand{
		CommandId: p.nextCommandID(),
		Command:   &v1.RunnerCommand_CreateSession{CreateSession: req},
	})
	if err != nil {
		return nil, err
	}
	resp := result.GetCreateSession()
	if resp == nil {
		return nil, errors.New("unexpected result type")
	}
	return resp, nil
}

func (p *BridgeProxy) SessionExec(ctx context.Context, req *v1.SessionExecRequest, onOutput OutputCallback) (*v1.SessionExecResponse, error) {
	cmd := &v1.RunnerCommand{
		CommandId: p.nextCommandID(),
		Command:   &v1.RunnerCommand_SessionExec{SessionExec: req},
	}

	w := p.registerWaiter(cmd.CommandId, true)
	defer p.removeWaiter(cmd.CommandId)

	select {
	case p.commandCh <- cmd:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	slog.Debug("session exec sent, waiting for result", "command_id", cmd.CommandId)

	// Loop on both outputCh and resultCh until we get the result.
	for {
		select {
		case chunk := <-w.outputCh:
			if onOutput != nil {
				onOutput(chunk.Stdout, chunk.Stderr)
			}
		case result := <-w.resultCh:
			// Drain any remaining output chunks (non-blocking).
			for {
				select {
				case chunk := <-w.outputCh:
					if onOutput != nil {
						onOutput(chunk.Stdout, chunk.Stderr)
					}
				default:
					goto drained
				}
			}
		drained:
			if result.Error != "" {
				return nil, fmt.Errorf("runner error: %s", result.Error)
			}
			resp := result.GetSessionExec()
			if resp == nil {
				return nil, errors.New("unexpected result type")
			}
			return resp, nil
		case <-ctx.Done():
			slog.Warn("session exec timed out", "command_id", cmd.CommandId)
			return nil, ctx.Err()
		}
	}
}

func (p *BridgeProxy) CloseSession(ctx context.Context, req *v1.CloseSessionRequest) (*v1.CloseSessionResponse, error) {
	result, err := p.sendAndWait(ctx, &v1.RunnerCommand{
		CommandId: p.nextCommandID(),
		Command:   &v1.RunnerCommand_CloseSession{CloseSession: req},
	})
	if err != nil {
		return nil, err
	}
	resp := result.GetCloseSession()
	if resp == nil {
		return nil, errors.New("unexpected result type")
	}
	return resp, nil
}

func (p *BridgeProxy) Exec(ctx context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
	result, err := p.sendAndWait(ctx, &v1.RunnerCommand{
		CommandId: p.nextCommandID(),
		Command:   &v1.RunnerCommand_Exec{Exec: req},
	})
	if err != nil {
		return nil, err
	}
	resp := result.GetExec()
	if resp == nil {
		return nil, errors.New("unexpected result type")
	}
	return resp, nil
}

func (p *BridgeProxy) UploadFiles(ctx context.Context, req *v1.UploadFilesRequest) (*v1.UploadFilesResponse, error) {
	result, err := p.sendAndWait(ctx, &v1.RunnerCommand{
		CommandId: p.nextCommandID(),
		Command:   &v1.RunnerCommand_UploadFiles{UploadFiles: req},
	})
	if err != nil {
		return nil, err
	}
	resp := result.GetUploadFiles()
	if resp == nil {
		return nil, errors.New("unexpected result type")
	}
	return resp, nil
}

func (p *BridgeProxy) ReadFile(ctx context.Context, req *v1.ReadFileRequest) (*v1.ReadFileResponse, error) {
	result, err := p.sendAndWait(ctx, &v1.RunnerCommand{
		CommandId: p.nextCommandID(),
		Command:   &v1.RunnerCommand_ReadFile{ReadFile: req},
	})
	if err != nil {
		return nil, err
	}
	resp := result.GetReadFile()
	if resp == nil {
		return nil, errors.New("unexpected result type")
	}
	return resp, nil
}

func (p *BridgeProxy) EditFile(ctx context.Context, req *v1.EditFileRequest) (*v1.EditFileResponse, error) {
	result, err := p.sendAndWait(ctx, &v1.RunnerCommand{
		CommandId: p.nextCommandID(),
		Command:   &v1.RunnerCommand_EditFile{EditFile: req},
	})
	if err != nil {
		return nil, err
	}
	resp := result.GetEditFile()
	if resp == nil {
		return nil, errors.New("unexpected result type")
	}
	return resp, nil
}

func (p *BridgeProxy) WriteFile(ctx context.Context, req *v1.WriteFileRequest) (*v1.WriteFileResponse, error) {
	result, err := p.sendAndWait(ctx, &v1.RunnerCommand{
		CommandId: p.nextCommandID(),
		Command:   &v1.RunnerCommand_WriteFile{WriteFile: req},
	})
	if err != nil {
		return nil, err
	}
	resp := result.GetWriteFile()
	if resp == nil {
		return nil, errors.New("unexpected result type")
	}
	return resp, nil
}

func (p *BridgeProxy) generateTransferID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate transfer ID: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// StreamToGuest streams data from src to a file at destPath in the guest VM.
// The orchestrator registers the transfer, tells the runner to call DownloadFile,
// and the handler streams the data.
func (p *BridgeProxy) StreamToGuest(ctx context.Context, destPath string, src io.Reader, size int64) error {
	transferID, err := p.generateTransferID()
	if err != nil {
		return err
	}

	rc, ok := src.(io.ReadCloser)
	if !ok {
		rc = io.NopCloser(src)
	}

	t := &dispatch.PendingTransfer{
		Reader: rc,
		Meta: &v1.FileStreamStart{
			TransferId: transferID,
			Path:       destPath,
			Size:       size,
			Mode:       0o644,
		},
		Done: make(chan struct{}),
	}
	p.runner.RegisterTransfer(transferID, t)

	// Tell the runner to call DownloadFile.
	if _, err = p.sendAndWait(ctx, &v1.RunnerCommand{
		CommandId: p.nextCommandID(),
		Command: &v1.RunnerCommand_DownloadFile{DownloadFile: &v1.DownloadFileCommand{
			TransferId: transferID,
			Path:       destPath,
		}},
	}); err != nil {
		p.runner.RemoveTransfer(transferID)
		return fmt.Errorf("stream to guest: %w", err)
	}

	return nil
}

// StreamFromGuest streams a file at srcPath from the guest VM to dest.
// The orchestrator creates a pipe, tells the runner to call UploadFile,
// and copies the pipe reader into dest.
func (p *BridgeProxy) StreamFromGuest(ctx context.Context, srcPath string, dest io.Writer) error {
	transferID, err := p.generateTransferID()
	if err != nil {
		return err
	}

	pr, pw := io.Pipe()
	t := &dispatch.PendingTransfer{
		Writer: pw,
		Meta: &v1.FileStreamStart{
			TransferId: transferID,
			Path:       srcPath,
		},
		Done: make(chan struct{}),
	}
	p.runner.RegisterTransfer(transferID, t)

	// Start copying data concurrently. The pipe has no internal buffer,
	// so the reader must be active before the runner starts writing —
	// otherwise sendAndWait deadlocks waiting for a result that can't
	// arrive until the upload finishes writing to the pipe.
	copyErrCh := make(chan error, 1)
	go func() {
		_, err := io.Copy(dest, pr)
		copyErrCh <- err
	}()

	// Tell the runner to call UploadFile.
	if _, err = p.sendAndWait(ctx, &v1.RunnerCommand{
		CommandId: p.nextCommandID(),
		Command: &v1.RunnerCommand_UploadFile{UploadFile: &v1.UploadFileCommand{
			TransferId: transferID,
			Path:       srcPath,
		}},
	}); err != nil {
		p.runner.RemoveTransfer(transferID)
		pr.Close()
		pw.Close()
		return fmt.Errorf("stream from guest: %w", err)
	}

	if err := <-copyErrCh; err != nil {
		return fmt.Errorf("copy from guest stream: %w", err)
	}

	return nil
}
