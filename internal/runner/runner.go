package runner

import (
	"bytes"
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

	"connectrpc.com/connect"
	"github.com/cockroachdb/errors"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// DefaultExecTimeout is the default exec timeout in seconds (5 minutes).
const DefaultExecTimeout uint32 = 300

// maxSessions is the maximum number of concurrent shell sessions per handler.
const maxSessions = 16

// Handler implements the runner service, handling both direct RPC calls and bridge commands.
type Handler struct {
	kvarnCred  *kvarnCredential // cached non-privileged user credentials (nil if lookup failed)
	sessions   map[string]*shellSession
	sessionMu  sync.Mutex
	nextSessID atomic.Int64
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

// Close terminates all active sessions. Should be called on disconnect.
func (h *Handler) Close() {
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
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.Newf("too many sessions (%d), limit is %d", count, maxSessions))
	}

	sess, err := newShellSession(msg.WorkingDir, msg.Container, h.kvarnCred)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.Wrap(err, "create shell session"))
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
		return nil, connect.NewError(connect.CodeNotFound, errors.Newf("session %q not found", msg.SessionId))
	}

	timeout := time.Duration(msg.TimeoutSeconds) * time.Second

	result, err := sess.Execute(ctx, msg.Command, timeout, onOutput)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, connect.NewError(connect.CodeDeadlineExceeded, errors.New("command timed out"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&v1.SessionExecResponse{
		ExitCode:   result.ExitCode,
		Stdout:     result.Stdout,
		Stderr:     result.Stderr,
		WorkingDir: result.Cwd,
		StateReset: result.StateReset,
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
		return nil, connect.NewError(connect.CodeNotFound, errors.Newf("session %q not found", msg.SessionId))
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

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	exitCode, err := resolveExitCode(ctx, err)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&v1.ExecResponse{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}), nil
}

// safePath resolves relPath within workingDir and ensures it doesn't escape.
func safePath(workingDir, relPath string) (string, error) {
	if relPath == "" {
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("path must not be empty"))
	}

	// Resolve the working directory fully (handles symlinks like macOS /tmp -> /private/tmp)
	absDir, err := filepath.Abs(workingDir)
	if err != nil {
		return "", err
	}
	absDir, err = filepath.EvalSymlinks(absDir)
	if err != nil {
		return "", err
	}

	// Clean the joined path without following symlinks first
	joined := filepath.Clean(filepath.Join(absDir, relPath))

	// Check the cleaned path stays within the working directory
	if !strings.HasPrefix(joined, absDir+string(filepath.Separator)) {
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("path escapes working directory"))
	}

	return joined, nil
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

		// Chown workspace files to the kvarn user so non-privileged commands can access them.
		if h.kvarnCred != nil && strings.HasPrefix(resolved, "/home/kvarn") {
			uid, gid := h.kvarnCred.chownIDs()
			// Lchown so we don't follow symlinks.
			os.Lchown(resolved, uid, gid)
			// Chown any directories created between the home directory and the file.
			for d := dir; d != "/home/kvarn" && strings.HasPrefix(d, "/home/kvarn/"); d = filepath.Dir(d) {
				os.Chown(d, uid, gid)
			}
		}

		// The kvarn-*.sh scripts in /etc/profile.d are sourced by the
		// kvarn user's login shell. Owning them as root would leave the
		// 0600 secrets script unreadable (kvarn-tools.sh and kvarn-user.sh
		// at 0644 would still work, but kvarn-secrets.sh would be skipped
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

	content, err := os.ReadFile(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, connect.NewError(connect.CodeNotFound, errors.Newf("file not found: %s", msg.Path))
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
			return nil, connect.NewError(connect.CodeNotFound, errors.Newf("file not found: %s", msg.Path))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	content, err := os.ReadFile(resolved)
	if err != nil {
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
				return nil, connect.NewError(connect.CodeNotFound, errors.Newf("file not found: %s", msg.Path))
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

	return connect.NewResponse(&v1.WriteFileResponse{
		Version:    hashFile(msg.Content),
		TotalLines: int32(len(lines)),
	}), nil
}

// resolveExitCode extracts a meaningful exit code from an exec error.
// For signal-killed processes it returns 128 + signal number (Unix convention).
// If the context deadline was exceeded it returns a DeadlineExceeded RPC error.
func resolveExitCode(ctx context.Context, err error) (int32, error) {
	if err == nil {
		return 0, nil
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 0, connect.NewError(connect.CodeInternal, err)
	}

	if ctx.Err() == context.DeadlineExceeded {
		return 0, connect.NewError(connect.CodeDeadlineExceeded, errors.New("command timed out"))
	}

	// ExitCode() returns -1 when the process was killed by a signal.
	// Extract the actual signal and use the Unix convention of 128 + signal.
	if exitErr.ExitCode() == -1 {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return int32(128 + status.Signal()), nil
		}
	}

	return int32(exitErr.ExitCode()), nil
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
