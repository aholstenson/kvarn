package runner

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"errors"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// DefaultExecTimeout is the default exec timeout in seconds (5 minutes).
const DefaultExecTimeout uint32 = 300

// execWaitDelay is how long a killed command has to release the output pipes
// before they are closed from under it. It only matters for a process that put
// itself outside the group the kill went to, which is rare enough that the
// delay is never paid in practice and short enough that it is not felt when it
// is.
const execWaitDelay = 2 * time.Second

// maxSessions is the maximum number of concurrent shell sessions per handler.
const maxSessions = 16

// kvarnHome is the home directory of the unprivileged user every job command
// runs as. The workspace lives under it, and so does everything a job shares
// with the containers it starts.
const kvarnHome = "/home/kvarn"

// Handler implements the runner service, handling both direct RPC calls and bridge commands.
type Handler struct {
	kvarnCred  *kvarnCredential // cached non-privileged user credentials (nil if lookup failed)
	sessions   map[string]*shellSession
	sessionMu  sync.Mutex
	nextSessID atomic.Int64

	// processes are the long-lived processes started through StartProcess.
	// They are separate from sessions because they outlive the call that
	// started them and are stopped by name rather than by returning.
	processes map[string]*managedProcess
	processMu sync.Mutex
}

// NewHandler creates a new handler that can be used to execute runner commands directly.
// It looks up the kvarn user and drops privileges for shell sessions when found.
func NewHandler() *Handler {
	h := &Handler{
		sessions: make(map[string]*shellSession),
	}
	cred, err := lookupKvarnUser()
	if err != nil {
		slog.Warn("failed to lookup kvarn user, all commands will run as current user", "error", err)
	} else {
		h.kvarnCred = cred
	}
	return h
}

// NewUnprivilegedHandler creates a handler that runs all commands as the
// current user without attempting privilege changes.
func NewUnprivilegedHandler() *Handler {
	return &Handler{
		sessions: make(map[string]*shellSession),
	}
}

// Close terminates all active sessions and long-lived processes. Should be
// called on disconnect: a process started over a bridge that has gone away has
// nobody left to stop it.
func (h *Handler) Close() {
	h.closeProcesses()

	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	for id, sess := range h.sessions {
		sess.Close()
		delete(h.sessions, id)
	}
}

func (h *Handler) CreateSession(_ context.Context, req *connect.Request[v1.CreateSessionRequest]) (*connect.Response[v1.CreateSessionResponse], error) {
	msg := req.Msg

	h.sessionMu.Lock()
	count := len(h.sessions)
	h.sessionMu.Unlock()

	if count >= maxSessions {
		return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("too many sessions (%d), limit is %d", count, maxSessions))
	}

	sess, err := newShellSession(msg.WorkingDir, h.kvarnCred)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create shell session: %w", err))
	}

	id := fmt.Sprintf("sess-%d", h.nextSessID.Add(1))

	h.sessionMu.Lock()
	h.sessions[id] = sess
	h.sessionMu.Unlock()

	return connect.NewResponse(&v1.CreateSessionResponse{
		SessionId: id,
	}), nil
}

func (h *Handler) SessionExec(ctx context.Context, req *connect.Request[v1.SessionExecRequest]) (*connect.Response[v1.SessionExecResponse], error) {
	return h.SessionExecWithOutput(ctx, req.Msg, nil)
}

// SessionExecWithOutput executes a command in a session, calling onOutput with
// incremental stdout/stderr chunks as they become available.
func (h *Handler) SessionExecWithOutput(ctx context.Context, msg *v1.SessionExecRequest, onOutput OutputCallback) (*connect.Response[v1.SessionExecResponse], error) {
	h.sessionMu.Lock()
	sess, ok := h.sessions[msg.SessionId]
	h.sessionMu.Unlock()

	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %q not found", msg.SessionId))
	}

	timeout := time.Duration(msg.TimeoutSeconds) * time.Second

	result, err := sess.Execute(ctx, msg.Command, timeout, int(msg.MaxOutputBytes), onOutput)
	// A command killed for running too long still produced everything it wrote
	// up to that point, and that output is the only account of what it was
	// doing. Reporting the timeout as a failed call would throw it away, so it
	// comes back as a result carrying TimedOut and the timeout exit code.
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&v1.SessionExecResponse{
		ExitCode:         result.ExitCode,
		Stdout:           result.Stdout,
		Stderr:           result.Stderr,
		WorkingDir:       result.Cwd,
		StateReset:       result.StateReset,
		TimedOut:         result.TimedOut,
		StdoutTotalBytes: uint64(result.StdoutTotal),
		StderrTotalBytes: uint64(result.StderrTotal),
	}), nil
}

func (h *Handler) CloseSession(_ context.Context, req *connect.Request[v1.CloseSessionRequest]) (*connect.Response[v1.CloseSessionResponse], error) {
	msg := req.Msg

	h.sessionMu.Lock()
	sess, ok := h.sessions[msg.SessionId]
	if ok {
		delete(h.sessions, msg.SessionId)
	}
	h.sessionMu.Unlock()

	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %q not found", msg.SessionId))
	}

	sess.Close()
	return connect.NewResponse(&v1.CloseSessionResponse{}), nil
}

func (h *Handler) Exec(ctx context.Context, req *connect.Request[v1.ExecRequest]) (*connect.Response[v1.ExecResponse], error) {
	msg := req.Msg

	timeout := msg.TimeoutSeconds
	if timeout == 0 {
		timeout = DefaultExecTimeout
	}
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	if !msg.Privileged && h.kvarnCred != nil {
		// Run as kvarn user via su -l, which sets up a proper login session
		// (HOME, XDG_RUNTIME_DIR via /etc/profile.d, PATH, etc).
		//
		// su -l changes to the user's home directory, so we prepend a cd
		// to ensure the command runs in the requested working directory.
		//
		// All su options must come BEFORE the username per POSIX convention.
		if len(msg.Args) == 0 {
			shellCmd := msg.Command
			if msg.WorkingDir != "" {
				shellCmd = fmt.Sprintf("cd %q && %s", msg.WorkingDir, msg.Command)
			}
			cmd = exec.CommandContext(ctx, "su", "-l", "-s", "/bin/sh", "-c", shellCmd, "--", "kvarn")
		} else {
			// Use "exec $@" pattern to avoid shell-escaping issues:
			// su runs sh -c 'exec "$@"' with the real command as positional args.
			// The "sh" before msg.Command is $0 (argv[0] for the inner shell).
			shellScript := `exec "$@"`
			if msg.WorkingDir != "" {
				shellScript = fmt.Sprintf("cd %q && exec \"$@\"", msg.WorkingDir)
			}
			args := []string{"-l", "-s", "/bin/sh", "-c", shellScript, "--", "kvarn", "sh", msg.Command}
			args = append(args, msg.Args...)
			cmd = exec.CommandContext(ctx, "su", args...)
		}
	} else if len(msg.Args) == 0 {
		// Privileged: run through a login shell for PATH from /etc/profile.d/*.sh.
		cmd = exec.CommandContext(ctx, "sh", "-l", "-c", msg.Command)
	} else {
		cmd = exec.CommandContext(ctx, msg.Command, msg.Args...)
	}
	if msg.WorkingDir != "" {
		cmd.Dir = msg.WorkingDir
	}

	// The timeout has to reach the whole tree, not just the process started
	// here. Commands arrive as pipelines and login shells, so what runs long is
	// usually a grandchild; killing only the leader leaves it running, still
	// holding the output pipe, and the call then waits out the very command the
	// timeout was meant to cut short. Its own process group makes the kill
	// cover everything descended from it, and WaitDelay bounds the wait for
	// anything that escapes the group anyway.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = execWaitDelay

	stdout := newCapBuffer(int(msg.MaxOutputBytes))
	stderr := newCapBuffer(int(msg.MaxOutputBytes))
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()

	exitCode, timedOut, err := resolveExitCode(ctx, err)
	if err != nil {
		return nil, err
	}

	resp := &v1.ExecResponse{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		TimedOut: timedOut,
	}
	if stdout.Truncated() {
		resp.StdoutTotalBytes = uint64(stdout.Total())
	}
	if stderr.Truncated() {
		resp.StderrTotalBytes = uint64(stderr.Total())
	}
	return connect.NewResponse(resp), nil
}

// safePath resolves path within workingDir and ensures it doesn't escape.
//
// path is normally relative to the working directory, but a model driving these
// tools regularly reaches for the full path it saw in a command's output
// instead. An absolute path naming a file inside the working directory is
// therefore accepted as the equivalent relative path; joining it blindly would
// double the prefix and report a file that plainly exists as missing. Both
// spellings of the directory are matched — the one the caller was handed and
// the one it resolves to — so a symlinked workspace still recognizes its own
// files. Anything genuinely outside is still rejected.
func safePath(workingDir, path string) (string, error) {
	if path == "" {
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("path must not be empty"))
	}

	// Resolve the working directory fully (handles symlinks like macOS /tmp -> /private/tmp)
	absDir, err := filepath.Abs(workingDir)
	if err != nil {
		return "", err
	}
	resolvedDir, err := filepath.EvalSymlinks(absDir)
	if err != nil {
		return "", err
	}

	rel := path
	if filepath.IsAbs(rel) {
		inside, ok := relativeToAny(rel, resolvedDir, absDir)
		if !ok {
			return "", connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("absolute path %s is outside the working directory %s; pass a path relative to it", path, absDir))
		}
		rel = inside
	}

	// Clean the joined path without following symlinks first
	joined := filepath.Clean(filepath.Join(resolvedDir, rel))

	// Check the cleaned path stays within the working directory
	if joined == resolvedDir {
		return "", connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("path %s is the working directory itself, not a file", path))
	}
	if !strings.HasPrefix(joined, withSeparator(resolvedDir)) {
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("path escapes working directory"))
	}

	return joined, nil
}

// relativeToAny returns path relative to the first of dirs that contains it,
// reporting whether any did. A path equal to one of the dirs yields ".".
func relativeToAny(path string, dirs ...string) (string, bool) {
	clean := filepath.Clean(path)
	for _, dir := range dirs {
		if clean == dir {
			return ".", true
		}
		if rest, ok := strings.CutPrefix(clean, withSeparator(dir)); ok {
			return rest, true
		}
	}
	return "", false
}

// withSeparator returns dir with a trailing separator so that prefix
// comparisons match whole path elements rather than partial names, leaving an
// already-terminated dir (such as the root) alone.
func withSeparator(dir string) string {
	if strings.HasSuffix(dir, string(filepath.Separator)) {
		return dir
	}
	return dir + string(filepath.Separator)
}

// chownToKvarn hands path, and any directory above it up to the kvarn home
// directory, to the kvarn user.
//
// Every write the runner performs on a job's behalf needs this, because the
// runner is root while the job is not: a file left owned by root cannot be
// rewritten by the steps that come after it, and rootless Podman maps the kvarn
// user to container root while leaving root itself outside the container's user
// namespace, so inside a container the same file surfaces as owned by nobody.
//
// createdDir is the deepest directory the caller may have created on the way to
// path, and is walked up to the home directory. Callers that created nothing
// pass an empty string rather than the file's parent: a directory that was
// already there may deliberately belong to someone else, such as a container
// user that wrote it through the bind mount.
func (h *Handler) chownToKvarn(path string, createdDir string) {
	if h.kvarnCred == nil || !isUnderKvarnHome(path) {
		return
	}
	uid, gid := h.kvarnCred.chownIDs()
	// Lchown so we don't follow symlinks.
	os.Lchown(path, uid, gid)
	for d := createdDir; isUnderKvarnHome(d); d = filepath.Dir(d) {
		os.Chown(d, uid, gid)
	}
}

// isUnderKvarnHome reports whether path is strictly inside the kvarn home
// directory, so the home directory itself is never reowned.
func isUnderKvarnHome(path string) bool {
	return strings.HasPrefix(path, kvarnHome+string(filepath.Separator))
}

func (h *Handler) UploadFiles(ctx context.Context, req *connect.Request[v1.UploadFilesRequest]) (*connect.Response[v1.UploadFilesResponse], error) {
	msg := req.Msg
	if msg.WorkingDir == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("working_dir is required"))
	}

	var count int32
	for _, f := range msg.Files {
		resolved, err := safePath(msg.WorkingDir, f.Path)
		if err != nil {
			return nil, err
		}

		dir := filepath.Dir(resolved)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}

		if f.SymlinkTarget != "" {
			// Remove any existing file/symlink at the path before creating.
			os.Remove(resolved)
			if err := os.Symlink(f.SymlinkTarget, resolved); err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
		} else {
			mode := fs.FileMode(f.Mode)
			if mode == 0 {
				mode = 0644
			}

			if err := os.WriteFile(resolved, f.Content, mode); err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
		}

		h.chownToKvarn(resolved, dir)

		// The scripts kvarn writes into /etc/profile.d are sourced by the
		// kvarn user's login shell. Owning them as root would leave the
		// 0600 secrets script unreadable (the 0644 environment and PATH
		// scripts would still work, but kvarn-secrets.sh would be skipped
		// by the `[ -r ]` guard in /etc/profile). Chown to kvarn so the
		// shell can actually read what we wrote. Root retains access.
		if h.kvarnCred != nil && strings.HasPrefix(resolved, "/etc/profile.d/") {
			uid, gid := h.kvarnCred.chownIDs()
			os.Lchown(resolved, uid, gid)
		}

		count++
	}

	return connect.NewResponse(&v1.UploadFilesResponse{
		FilesWritten: count,
	}), nil
}

func (h *Handler) ReadFile(ctx context.Context, req *connect.Request[v1.ReadFileRequest]) (*connect.Response[v1.ReadFileResponse], error) {
	msg := req.Msg
	if msg.WorkingDir == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("working_dir is required"))
	}

	resolved, err := safePath(msg.WorkingDir, msg.Path)
	if err != nil {
		return nil, err
	}

	content, err := readAnchorableFile(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("file not found: %s", msg.Path))
		}
		var ae *AnchoredError
		if errors.As(err, &ae) {
			return nil, ae.toConnectError()
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	resp, anchoredErr := buildReadResponse(content, int(msg.StartLine), int(msg.EndLine))
	if anchoredErr != nil {
		return nil, anchoredErr.toConnectError()
	}
	return connect.NewResponse(resp), nil
}

// buildReadResponse runs the read-side anchoring logic and returns the proto
// response (or a structured error). Split out so EditFile can reuse it when
// attaching fresh snapshots to mismatch errors.
func buildReadResponse(content []byte, startLine, endLine int) (*v1.ReadFileResponse, *AnchoredError) {
	if err := validateFileContent(content); err != nil {
		if ae, ok := err.(*AnchoredError); ok {
			return nil, ae
		}
		return nil, &AnchoredError{Code: ErrFileEncoding, Detail: err.Error()}
	}
	lines, newline, _, splitErr := splitLines(content)
	if splitErr != nil {
		if ae, ok := splitErr.(*AnchoredError); ok {
			return nil, ae
		}
		return nil, &AnchoredError{Code: ErrMixedNewline, Detail: splitErr.Error()}
	}

	total := len(lines)
	wStart := 1
	wEnd := total
	if startLine > 0 {
		wStart = startLine
	}
	if endLine > 0 {
		wEnd = endLine
	}
	if total == 0 {
		wStart, wEnd = 1, 0
	} else {
		if wStart < 1 {
			wStart = 1
		}
		if wEnd > total {
			wEnd = total
		}
		if wStart > total {
			wStart = total
			wEnd = total - 1
		}
	}

	var window [][]byte
	if total > 0 && wStart <= wEnd {
		window = lines[wStart-1 : wEnd]
	}
	tagged, _ := tagLines(lines, wStart, window)

	return &v1.ReadFileResponse{
		Version:    hashFile(content),
		TotalLines: int32(total),
		Lines:      tagged,
		Newline:    newline,
	}, nil
}

func (h *Handler) EditFile(ctx context.Context, req *connect.Request[v1.EditFileRequest]) (*connect.Response[v1.EditFileResponse], error) {
	msg := req.Msg
	if msg.WorkingDir == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("working_dir is required"))
	}
	if len(msg.Operations) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("operations must not be empty"))
	}

	resolved, err := safePath(msg.WorkingDir, msg.Path)
	if err != nil {
		return nil, err
	}

	mu := pathMutex(resolved)
	mu.Lock()
	defer mu.Unlock()

	info, err := os.Stat(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("file not found: %s", msg.Path))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	content, err := readAnchorableFile(resolved)
	if err != nil {
		var ae *AnchoredError
		if errors.As(err, &ae) {
			return nil, ae.toConnectError()
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if vErr := validateFileContent(content); vErr != nil {
		if ae, ok := vErr.(*AnchoredError); ok {
			return nil, ae.toConnectError()
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, vErr)
	}

	lines, newline, trailingNewline, splitErr := splitLines(content)
	if splitErr != nil {
		if ae, ok := splitErr.(*AnchoredError); ok {
			return nil, ae.toConnectError()
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, splitErr)
	}

	currentVersion := hashFile(content)
	versionDrifted := msg.ExpectedVersion != "" && msg.ExpectedVersion != currentVersion

	totalLines := len(lines)

	// Bounds-check every op up front.
	for i, op := range msg.Operations {
		if op.Op == v1.EditOp_EDIT_OP_UNSPECIFIED {
			ae := &AnchoredError{Code: ErrInvalidOperation, Detail: fmt.Sprintf("operation %d has unspecified op code", i)}
			return nil, ae.toConnectError()
		}
		if err := validateOpBounds(op, i, totalLines); err != nil {
			if ae, ok := err.(*AnchoredError); ok {
				return nil, ae.toConnectError()
			}
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}

	// Resolve each op's anchor(s) into concrete 1-indexed line positions. The
	// op's `line` field is an optional tiebreaker used only when the anchor is
	// ambiguous; an INSERT_AFTER with line=0 still means "top of file".
	resolvedStart := make([]int, len(msg.Operations))
	resolvedEnd := make([]int, len(msg.Operations))
	for i, op := range msg.Operations {
		switch op.Op {
		case v1.EditOp_EDIT_OP_REPLACE, v1.EditOp_EDIT_OP_DELETE:
			if op.Hash == "" {
				ae := &AnchoredError{Code: ErrInvalidOperation, Detail: fmt.Sprintf("operation %d missing hash", i)}
				return nil, ae.toConnectError()
			}
			line, ae := resolveAnchor(lines, op.Hash, int(op.Line))
			if ae != nil {
				ae.Snapshot, _ = buildReadResponse(content, 0, 0)
				ae.Detail = fmt.Sprintf("operation %d: %s", i, ae.Detail)
				return nil, ae.toConnectError()
			}
			resolvedStart[i] = line
			resolvedEnd[i] = line
		case v1.EditOp_EDIT_OP_INSERT_AFTER:
			if op.Line == 0 && op.Hash == "" {
				// Insert at top of file: no anchor needed.
				resolvedStart[i] = 0
				resolvedEnd[i] = 0
				continue
			}
			if op.Hash == "" {
				ae := &AnchoredError{Code: ErrInvalidOperation, Detail: fmt.Sprintf("operation %d missing hash", i)}
				return nil, ae.toConnectError()
			}
			line, ae := resolveAnchor(lines, op.Hash, int(op.Line))
			if ae != nil {
				ae.Snapshot, _ = buildReadResponse(content, 0, 0)
				ae.Detail = fmt.Sprintf("operation %d: %s", i, ae.Detail)
				return nil, ae.toConnectError()
			}
			resolvedStart[i] = line
			resolvedEnd[i] = line
		case v1.EditOp_EDIT_OP_INSERT_BEFORE:
			if op.Hash == "" {
				ae := &AnchoredError{Code: ErrInvalidOperation, Detail: fmt.Sprintf("operation %d missing hash", i)}
				return nil, ae.toConnectError()
			}
			line, ae := resolveAnchor(lines, op.Hash, int(op.Line))
			if ae != nil {
				ae.Snapshot, _ = buildReadResponse(content, 0, 0)
				ae.Detail = fmt.Sprintf("operation %d: %s", i, ae.Detail)
				return nil, ae.toConnectError()
			}
			resolvedStart[i] = line
			resolvedEnd[i] = line
		case v1.EditOp_EDIT_OP_REPLACE_RANGE, v1.EditOp_EDIT_OP_DELETE_RANGE:
			if op.StartHash == "" || op.EndHash == "" {
				ae := &AnchoredError{Code: ErrInvalidOperation, Detail: fmt.Sprintf("operation %d missing start_hash or end_hash", i)}
				return nil, ae.toConnectError()
			}
			startLine, ae := resolveAnchor(lines, op.StartHash, int(op.StartLine))
			if ae != nil {
				ae.Snapshot, _ = buildReadResponse(content, 0, 0)
				ae.Detail = fmt.Sprintf("operation %d start: %s", i, ae.Detail)
				return nil, ae.toConnectError()
			}
			endLine, ae := resolveAnchor(lines, op.EndHash, int(op.EndLine))
			if ae != nil {
				ae.Snapshot, _ = buildReadResponse(content, 0, 0)
				ae.Detail = fmt.Sprintf("operation %d end: %s", i, ae.Detail)
				return nil, ae.toConnectError()
			}
			if startLine > endLine {
				ae := &AnchoredError{Code: ErrInvalidOperation, Detail: fmt.Sprintf("operation %d has start_line > end_line after anchor resolution", i)}
				return nil, ae.toConnectError()
			}
			resolvedStart[i] = startLine
			resolvedEnd[i] = endLine
		}
	}

	if _, err := buildIntervals(msg.Operations, resolvedStart, resolvedEnd); err != nil {
		if ae, ok := err.(*AnchoredError); ok {
			return nil, ae.toConnectError()
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	// Capture original anchor lines for context computation after the edit.
	anchorRecords := make([]int, 0, len(msg.Operations))
	for i := range msg.Operations {
		anchorRecords = append(anchorRecords, resolvedStart[i])
	}

	// Apply ops in descending start order so earlier indices remain valid.
	indices := make([]int, len(msg.Operations))
	for i := range msg.Operations {
		indices[i] = i
	}
	sort.Slice(indices, func(i, j int) bool {
		return resolvedStart[indices[i]] > resolvedStart[indices[j]]
	})

	newLines := make([][]byte, len(lines))
	copy(newLines, lines)

	for _, idx := range indices {
		op := msg.Operations[idx]
		switch op.Op {
		case v1.EditOp_EDIT_OP_REPLACE:
			repl := stringsToByteSlices(op.Lines)
			newLines = spliceLines(newLines, resolvedStart[idx]-1, resolvedStart[idx], repl)
		case v1.EditOp_EDIT_OP_REPLACE_RANGE:
			repl := stringsToByteSlices(op.Lines)
			newLines = spliceLines(newLines, resolvedStart[idx]-1, resolvedEnd[idx], repl)
		case v1.EditOp_EDIT_OP_INSERT_AFTER:
			repl := stringsToByteSlices(op.Lines)
			newLines = spliceLines(newLines, resolvedStart[idx], resolvedStart[idx], repl)
		case v1.EditOp_EDIT_OP_INSERT_BEFORE:
			repl := stringsToByteSlices(op.Lines)
			newLines = spliceLines(newLines, resolvedStart[idx]-1, resolvedStart[idx]-1, repl)
		case v1.EditOp_EDIT_OP_DELETE:
			newLines = spliceLines(newLines, resolvedStart[idx]-1, resolvedStart[idx], nil)
		case v1.EditOp_EDIT_OP_DELETE_RANGE:
			newLines = spliceLines(newLines, resolvedStart[idx]-1, resolvedEnd[idx], nil)
		}
	}

	updated := joinLines(newLines, newline, trailingNewline || (len(lines) == 0))
	if len(lines) == 0 {
		// No prior content; treat trailing newline as off so we don't introduce one
		// unless the replacement text included it via op.Lines.
		updated = joinLines(newLines, newline, false)
	}

	if err := writeFileAtomic(resolved, updated, info.Mode()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// The atomic write replaces the file with one the runner created, so the
	// edit would otherwise hand a workspace file to root.
	h.chownToKvarn(resolved, "")

	contextLines := int(msg.ContextLines)
	if contextLines <= 0 {
		contextLines = defaultContextLines
	}

	contextSet := make([]*v1.TaggedLine, 0)
	seen := make(map[int32]bool)
	for _, line := range anchorRecords {
		tags := contextWindow(newLines, line, contextLines)
		for _, t := range tags {
			if !seen[t.Line] {
				seen[t.Line] = true
				contextSet = append(contextSet, t)
			}
		}
	}
	sort.Slice(contextSet, func(i, j int) bool { return contextSet[i].Line < contextSet[j].Line })

	return connect.NewResponse(&v1.EditFileResponse{
		Version:      hashFile(updated),
		TotalLines:   int32(len(newLines)),
		Context:      contextSet,
		VersionDrift: versionDrifted,
	}), nil
}

// spliceLines returns lines[:start] + replacement + lines[end:].
func spliceLines(lines [][]byte, start, end int, replacement [][]byte) [][]byte {
	if start < 0 {
		start = 0
	}
	if end > len(lines) {
		end = len(lines)
	}
	out := make([][]byte, 0, len(lines)-(end-start)+len(replacement))
	out = append(out, lines[:start]...)
	for _, r := range replacement {
		buf := make([]byte, len(r))
		copy(buf, r)
		out = append(out, buf)
	}
	out = append(out, lines[end:]...)
	return out
}

func stringsToByteSlices(ss []string) [][]byte {
	out := make([][]byte, len(ss))
	for i, s := range ss {
		out[i] = []byte(s)
	}
	return out
}

func (h *Handler) WriteFile(ctx context.Context, req *connect.Request[v1.WriteFileRequest]) (*connect.Response[v1.WriteFileResponse], error) {
	msg := req.Msg
	if msg.WorkingDir == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("working_dir is required"))
	}

	resolved, err := safePath(msg.WorkingDir, msg.Path)
	if err != nil {
		return nil, err
	}

	mu := pathMutex(resolved)
	mu.Lock()
	defer mu.Unlock()

	if vErr := validateFileContent(msg.Content); vErr != nil {
		if ae, ok := vErr.(*AnchoredError); ok {
			return nil, ae.toConnectError()
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, vErr)
	}
	lines, _, _, splitErr := splitLines(msg.Content)
	if splitErr != nil {
		if ae, ok := splitErr.(*AnchoredError); ok {
			return nil, ae.toConnectError()
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, splitErr)
	}

	mode := fs.FileMode(msg.Mode)
	if mode == 0 {
		mode = 0o644
	}

	dir := filepath.Dir(resolved)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	existing, statErr := os.Stat(resolved)
	if msg.ExpectedVersion == "" {
		if statErr == nil {
			ae := &AnchoredError{Code: ErrInvalidOperation, Detail: "file exists; pass expected_version to overwrite"}
			return nil, ae.toConnectError()
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return nil, connect.NewError(connect.CodeInternal, statErr)
		}
	} else {
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("file not found: %s", msg.Path))
			}
			return nil, connect.NewError(connect.CodeInternal, statErr)
		}
		existingContent, readErr := os.ReadFile(resolved)
		if readErr != nil {
			return nil, connect.NewError(connect.CodeInternal, readErr)
		}
		current := hashFile(existingContent)
		if current != msg.ExpectedVersion {
			snap, _ := buildReadResponse(existingContent, 0, 0)
			ae := &AnchoredError{
				Code:     ErrVersionConflict,
				Detail:   fmt.Sprintf("expected version %s, current version %s", msg.ExpectedVersion, current),
				Snapshot: snap,
			}
			return nil, ae.toConnectError()
		}
		mode = existing.Mode().Perm()
		if msg.Mode != 0 {
			mode = fs.FileMode(msg.Mode)
		}
	}

	if err := writeFileAtomic(resolved, msg.Content, mode); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	h.chownToKvarn(resolved, dir)

	return connect.NewResponse(&v1.WriteFileResponse{
		Version:    hashFile(msg.Content),
		TotalLines: int32(len(lines)),
	}), nil
}

// resolveExitCode extracts a meaningful exit code from an exec error, and
// reports whether the command was killed for running past its deadline rather
// than exiting on its own. For signal-killed processes it returns 128 + signal
// number (Unix convention).
func resolveExitCode(ctx context.Context, err error) (code int32, timedOut bool, _ error) {
	if err == nil {
		return 0, false, nil
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 0, false, connect.NewError(connect.CodeInternal, err)
	}

	// A timeout is an outcome of the command, not a failure of the call: what
	// it printed before being killed is the caller's only account of what it
	// was doing, and an error would discard it.
	if ctx.Err() == context.DeadlineExceeded {
		return timeoutExitCode, true, nil
	}

	// ExitCode() returns -1 when the process was killed by a signal.
	// Extract the actual signal and use the Unix convention of 128 + signal.
	if exitErr.ExitCode() == -1 {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return int32(128 + status.Signal()), false, nil
		}
	}

	return int32(exitErr.ExitCode()), false, nil
}

// NewServer creates an HTTP server with the runner service registered.
func NewServer() *http.Server {
	mux := http.NewServeMux()
	path, svcHandler := kvarnv1connect.NewRunnerServiceHandler(NewHandler())
	mux.Handle(path, svcHandler)
	return &http.Server{
		Handler: h2c.NewHandler(mux, &http2.Server{}),
	}
}

func run(addr string) error {
	srv := NewServer()
	srv.Addr = addr
	slog.Info("runner listening", "addr", addr)
	return srv.ListenAndServe()
}
