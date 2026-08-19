package runner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"errors"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
)

// defaultStopGrace is how long a process group is given to exit after SIGTERM
// before it is killed outright. Long enough for a web server to finish the
// requests it is holding, short enough that a stop is not mistaken for a hang.
const defaultStopGrace = 10 * time.Second

// maxProcesses bounds how many long-lived processes one handler supervises.
// Each holds a process group and two reader goroutines, and a preview declares
// a handful of services at most; a runaway caller should hit a limit rather
// than the VM's process table.
const maxProcesses = 32

// processOutputChunk is the largest slice of a process's output delivered in
// one callback. Servers log continuously, so output is forwarded in bounded
// pieces as it arrives rather than accumulated.
const processOutputChunk = 16 * 1024

// ProcessExitCallback is invoked once, off the caller's goroutine, when a
// managed process ends. exitCode follows the Unix convention of 128+signal for
// a process a signal killed.
type ProcessExitCallback func(processID string, exitCode int32, err error)

// ProcessOutputCallback receives incremental output from a managed process.
// Exactly one of stdout/stderr is non-empty per call.
type ProcessOutputCallback func(processID string, stdout, stderr string)

// managedProcess is one long-lived process the handler supervises.
type managedProcess struct {
	id   string
	name string
	cmd  *exec.Cmd
	pid  int

	// done is closed once the process has been reaped and exitCode is final.
	done chan struct{}

	mu       sync.Mutex
	exited   bool
	exitCode int32
}

// finished reports whether the process has been reaped, and its exit code.
func (p *managedProcess) finished() (bool, int32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exited, p.exitCode
}

// StartProcess implements the RunnerService RPC. Output and exit are dropped:
// a caller reaching the runner directly has no channel to receive them on. The
// bridge path uses StartProcessWithCallbacks instead.
func (h *Handler) StartProcess(ctx context.Context, req *connect.Request[v1.StartProcessRequest]) (*connect.Response[v1.StartProcessResponse], error) {
	return h.StartProcessWithCallbacks(ctx, req.Msg, nil, nil)
}

// StartProcessWithCallbacks spawns a process that outlives this call, wiring
// its output and eventual exit to the supplied callbacks.
//
// The process runs as the same unprivileged user every job command runs as,
// through a login shell so it inherits the curated PATH and environment the
// sandbox wrote to /etc/profile.d. It is given its own process group, which is
// what makes stopping it a group signal rather than a signal to one shell that
// leaves the real server behind.
func (h *Handler) StartProcessWithCallbacks(_ context.Context, msg *v1.StartProcessRequest, onOutput ProcessOutputCallback, onExit ProcessExitCallback) (*connect.Response[v1.StartProcessResponse], error) {
	if msg.ProcessId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("process_id is required"))
	}
	if strings.TrimSpace(msg.Command) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("command is required"))
	}

	h.processMu.Lock()
	if h.processes == nil {
		h.processes = make(map[string]*managedProcess)
	}
	if _, exists := h.processes[msg.ProcessId]; exists {
		h.processMu.Unlock()
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("process %q already exists", msg.ProcessId))
	}
	if len(h.processes) >= maxProcesses {
		h.processMu.Unlock()
		return nil, connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("too many processes (%d), limit is %d", len(h.processes), maxProcesses))
	}
	h.processMu.Unlock()

	script := processScript(msg.Command, msg.WorkingDir, msg.Env)

	var cmd *exec.Cmd
	if h.kvarnCred != nil {
		cmd = exec.Command("su", "-l", "-s", "/bin/sh", "-c", script, "--", "kvarn")
	} else {
		cmd = exec.Command("sh", "-l", "-c", script)
	}
	// Own process group: the shell exec's into the server, but anything it
	// forks along the way is only reachable as a group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("stdout pipe: %w", err))
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("stderr pipe: %w", err))
	}

	if err := cmd.Start(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("start process: %w", err))
	}

	proc := &managedProcess{
		id:   msg.ProcessId,
		name: msg.Name,
		cmd:  cmd,
		pid:  cmd.Process.Pid,
		done: make(chan struct{}),
	}

	h.processMu.Lock()
	h.processes[proc.id] = proc
	h.processMu.Unlock()

	// The two pumps must finish before Wait, or Wait closes the pipes out from
	// under them and the process's last lines are lost.
	var pumps sync.WaitGroup
	pumps.Add(2)
	go func() {
		defer pumps.Done()
		pumpProcessOutput(stdout, func(chunk string) {
			if onOutput != nil {
				onOutput(proc.id, chunk, "")
			}
		})
	}()
	go func() {
		defer pumps.Done()
		pumpProcessOutput(stderr, func(chunk string) {
			if onOutput != nil {
				onOutput(proc.id, "", chunk)
			}
		})
	}()

	go func() {
		pumps.Wait()
		waitErr := cmd.Wait()
		code := processExitCode(waitErr)

		proc.mu.Lock()
		proc.exited = true
		proc.exitCode = code
		proc.mu.Unlock()
		close(proc.done)

		if onExit != nil {
			onExit(proc.id, code, nil)
		}
	}()

	return connect.NewResponse(&v1.StartProcessResponse{
		ProcessId: proc.id,
		Pid:       int32(proc.pid),
	}), nil
}

// StopProcess signals a managed process's group and waits for it to go.
func (h *Handler) StopProcess(ctx context.Context, req *connect.Request[v1.StopProcessRequest]) (*connect.Response[v1.StopProcessResponse], error) {
	msg := req.Msg

	h.processMu.Lock()
	proc, ok := h.processes[msg.ProcessId]
	if ok {
		delete(h.processes, msg.ProcessId)
	}
	h.processMu.Unlock()

	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("process %q not found", msg.ProcessId))
	}

	grace := time.Duration(msg.GraceSeconds) * time.Second
	if grace <= 0 {
		grace = defaultStopGrace
	}

	code := stopProcess(proc, grace)
	return connect.NewResponse(&v1.StopProcessResponse{ExitCode: code}), nil
}

// ListProcesses reports every process this handler is supervising, plus those
// that have exited but not yet been stopped, so a caller can tell "still
// starting" from "crashed on boot".
func (h *Handler) ListProcesses(_ context.Context, _ *connect.Request[v1.ListProcessesRequest]) (*connect.Response[v1.ListProcessesResponse], error) {
	h.processMu.Lock()
	procs := make([]*managedProcess, 0, len(h.processes))
	for _, p := range h.processes {
		procs = append(procs, p)
	}
	h.processMu.Unlock()

	out := make([]*v1.ProcessInfo, 0, len(procs))
	for _, p := range procs {
		exited, code := p.finished()
		out = append(out, &v1.ProcessInfo{
			ProcessId: p.id,
			Name:      p.name,
			Pid:       int32(p.pid),
			Running:   !exited,
			ExitCode:  code,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProcessId < out[j].ProcessId })

	return connect.NewResponse(&v1.ListProcessesResponse{Processes: out}), nil
}

// closeProcesses stops every supervised process. Called from Handler.Close so
// a dropped bridge connection does not leave servers running in the guest.
func (h *Handler) closeProcesses() {
	h.processMu.Lock()
	procs := make([]*managedProcess, 0, len(h.processes))
	for id, p := range h.processes {
		procs = append(procs, p)
		delete(h.processes, id)
	}
	h.processMu.Unlock()

	for _, p := range procs {
		stopProcess(p, defaultStopGrace)
	}
}

// stopProcess terminates a process group: SIGTERM, then SIGKILL once grace has
// run out. It returns the exit code the process finally reported, or -1 if it
// could not be reaped.
func stopProcess(proc *managedProcess, grace time.Duration) int32 {
	if exited, code := proc.finished(); exited {
		return code
	}

	// Signal the group rather than the leader: the shell may have forked
	// helpers the server depends on, and those are not reachable by pid.
	_ = syscall.Kill(-proc.pid, syscall.SIGTERM)

	select {
	case <-proc.done:
	case <-time.After(grace):
		_ = syscall.Kill(-proc.pid, syscall.SIGKILL)
		select {
		case <-proc.done:
		case <-time.After(5 * time.Second):
			// The reaper goroutine is wedged on a pipe that will not close;
			// nothing more we can do without blocking the caller forever.
			return -1
		}
	}

	_, code := proc.finished()
	return code
}

// pumpProcessOutput forwards a process's output in bounded chunks as it
// arrives. A server runs for hours, so nothing is accumulated here: whoever
// receives the chunks decides how much to keep.
func pumpProcessOutput(r io.Reader, emit func(string)) {
	reader := bufio.NewReaderSize(r, processOutputChunk)
	buf := make([]byte, processOutputChunk)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			emit(string(buf[:n]))
		}
		if err != nil {
			return
		}
	}
}

// processScript builds the shell script the process runs under. The command is
// run as-is rather than exec'd: `run:` is a shell fragment, and a pipeline or a
// `&&` chain is not something exec can replace the shell with. Nothing is lost
// by the extra layer, because stopping a process signals the whole group.
func processScript(command, workingDir string, env map[string]string) string {
	var b strings.Builder
	if workingDir != "" {
		fmt.Fprintf(&b, "cd %s || exit 1\n", shellQuotePath(workingDir))
	}
	// Sorted so the script is identical for identical input, which keeps a
	// failure reproducible from the logs.
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&b, "export %s=%s\n", name, shellQuotePath(env[name]))
	}
	b.WriteString(command)
	b.WriteString("\n")
	return b.String()
}

// processExitCode turns the error from cmd.Wait into an exit status, using the
// Unix 128+signal convention for a process a signal ended — which is the normal
// outcome here, since stopping one is a SIGTERM.
func processExitCode(err error) int32 {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return -1
	}
	if exitErr.ExitCode() == -1 {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return int32(128 + status.Signal())
		}
	}
	return int32(exitErr.ExitCode())
}
