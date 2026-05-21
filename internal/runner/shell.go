package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// shellSession manages a persistent shell process with file-based output demarcation.
type shellSession struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	waitCh  chan struct{} // closed when cmd.Wait() returns
	mu      sync.Mutex   // serializes command execution
	tempDir string       // directory for output demarcation files
	nextID  atomic.Int64

	// Fields for respawning on shell death.
	initialDir string
	container  string
	kvarnCred  *kvarnCredential
}

func newShellSession(initialDir string, container string, kvarnCred *kvarnCredential) (*shellSession, error) {
	s := &shellSession{
		initialDir: initialDir,
		container:  container,
		kvarnCred:  kvarnCred,
	}

	tmpDir, err := os.MkdirTemp("", "kvarn-shell-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	s.tempDir = tmpDir

	// When running as the kvarn user, the shell needs read/write access to
	// the temp directory for demarcation files.
	if kvarnCred != nil {
		uid, gid := kvarnCred.chownIDs()
		if err := os.Chown(tmpDir, uid, gid); err != nil {
			os.RemoveAll(tmpDir)
			return nil, fmt.Errorf("chown temp dir: %w", err)
		}
	}

	if err := s.spawn(); err != nil {
		os.RemoveAll(tmpDir)
		return nil, err
	}

	return s, nil
}

func (s *shellSession) spawn() error {
	if s.container != "" {
		s.cmd = exec.Command("podman", "exec", "-i", s.container, "sh", "-l")
	} else if s.kvarnCred != nil {
		s.cmd = exec.Command("su", "-l", "-s", "/bin/sh", "--", "kvarn")
	} else {
		s.cmd = exec.Command("sh", "-l")
	}

	// Put the shell in its own process group so killAndRespawn can kill the
	// entire tree (shell + any child processes) rather than just the leader.
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var err error
	s.stdin, err = s.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}

	// Discard stdout/stderr from the shell process itself;
	// all output is captured via temp files.
	s.cmd.Stdout = io.Discard
	s.cmd.Stderr = io.Discard

	if err := s.cmd.Start(); err != nil {
		return fmt.Errorf("start shell: %w", err)
	}

	// Track shell death asynchronously.
	s.waitCh = make(chan struct{})
	go func() {
		s.cmd.Wait()
		close(s.waitCh)
	}()

	// Probe: verify the shell is alive by running a trivial command.
	// Set umask to restrict temp file permissions to owner-only.
	probeID := s.nextID.Add(1)

	// If initialDir is set, cd to it so commands run in the expected directory.
	var cdPrefix string
	if s.initialDir != "" {
		cdPrefix = fmt.Sprintf("cd %s 2>/dev/null; ", shellQuotePath(s.initialDir))
	}

	probeScript := fmt.Sprintf(
		"%sumask 0077; echo 0 >%s/%d.status\n",
		cdPrefix, s.tempDir, probeID,
	)
	if _, err := io.WriteString(s.stdin, probeScript); err != nil {
		s.killProcess()
		<-s.waitCh
		return fmt.Errorf("shell probe write: %w", err)
	}

	// Wait for probe status file.
	statusPath := filepath.Join(s.tempDir, fmt.Sprintf("%d.status", probeID))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(statusPath); err == nil {
			os.Remove(statusPath)
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}

	s.killProcess()
	<-s.waitCh
	return fmt.Errorf("shell probe timed out")
}

// killProcess kills the shell's entire process group.
func (s *shellSession) killProcess() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	// Kill the entire process group (negative PID) to clean up children.
	syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
}

// killAndRespawn kills the current shell and spawns a new one.
func (s *shellSession) killAndRespawn() error {
	if s.stdin != nil {
		s.stdin.Close()
	}
	s.killProcess()
	if s.waitCh != nil {
		<-s.waitCh
	}
	return s.spawn()
}

// OutputCallback is called with new stdout/stderr content as it becomes available.
type OutputCallback func(stdout, stderr string)

// executeResult holds the output of a shell command execution.
type executeResult struct {
	Stdout     string
	Stderr     string
	ExitCode   int32
	Cwd        string
	StateReset bool // true if the shell died and was respawned (state lost)
}

// Execute runs a command in the persistent shell and returns its output.
func (s *shellSession) Execute(ctx context.Context, command string, timeout time.Duration, onOutput OutputCallback) (result executeResult, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if timeout <= 0 {
		timeout = time.Duration(DefaultExecTimeout) * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	id := s.nextID.Add(1)
	prefix := filepath.Join(s.tempDir, fmt.Sprintf("%d", id))

	// Write the command to a temp file to avoid shell injection via the
	// demarcation script. The demarcation wrapper sources this file.
	// Mode 0644 so the kvarn user can read files written by root.
	cmdFile := prefix + ".cmd"
	if writeErr := os.WriteFile(cmdFile, []byte(command), 0644); writeErr != nil {
		err = fmt.Errorf("write command file: %w", writeErr)
		return
	}

	// Build the demarcation script. Redirect stdout/stderr to files,
	// source the command file, capture exit status and cwd, then restore.
	//
	// The command runs directly in the shell (not a subshell) so that
	// environment mutations (export, cd, source) persist across calls.
	// If the command calls `exit`, the shell dies and we respawn.
	//
	// We save and restore `set -e` state around the demarcation commands
	// so that user scripts that enable errexit don't kill the shell when
	// the demarcation bookkeeping runs.
	// The AND-OR list (`. file && __st=0 || __st=$?`) prevents `set -e`
	// from killing the shell when the sourced command fails. POSIX
	// specifies that `set -e` is suppressed for commands in AND-OR lists,
	// so the shell survives a non-zero exit and we can capture the code.
	script := fmt.Sprintf(
		"{\n"+
			"exec 1>%s.stdout 2>%s.stderr\n"+
			". %s && __st=0 || __st=$?\n"+
			"case $- in *e*) __had_e=1; set +e;; *) __had_e=0;; esac\n"+
			"pwd >%s.pwd\n"+
			"echo $__st >%s.status\n"+
			"[ \"$__had_e\" = 1 ] && set -e\n"+
			"exec 1>/dev/null 2>/dev/null\n"+
			"}\n",
		prefix, prefix, cmdFile, prefix, prefix,
	)

	if _, writeErr := io.WriteString(s.stdin, script); writeErr != nil {
		// Shell may have died; try to respawn once.
		if respawnErr := s.killAndRespawn(); respawnErr != nil {
			err = fmt.Errorf("shell died and respawn failed: %w", respawnErr)
			return
		}
		result.StateReset = true
		// Retry the write after respawn.
		if _, writeErr = io.WriteString(s.stdin, script); writeErr != nil {
			err = fmt.Errorf("write to shell after respawn: %w", writeErr)
			return
		}
	}

	// Poll for status file or shell death, tailing output files as they grow.
	statusPath := prefix + ".status"
	var stdoutOffset, stderrOffset int64
	for {
		if _, statErr := os.Stat(statusPath); statErr == nil {
			// Deliver any remaining output before returning.
			s.deliverNewOutput(prefix, &stdoutOffset, &stderrOffset, onOutput)
			break
		}

		s.deliverNewOutput(prefix, &stdoutOffset, &stderrOffset, onOutput)

		select {
		case <-s.waitCh:
			// Shell died (e.g., user command called `exit`).
			s.deliverNewOutput(prefix, &stdoutOffset, &stderrOffset, onOutput)

			// Read full output for the result.
			stdoutBytes, _ := os.ReadFile(prefix + ".stdout")
			stderrBytes, _ := os.ReadFile(prefix + ".stderr")
			result.Stdout = string(stdoutBytes)
			result.Stderr = string(stderrBytes)
			s.cleanupFiles(prefix)

			// Capture exit code from the dead process before respawning.
			result.ExitCode = 1
			if ps := s.cmd.ProcessState; ps != nil {
				result.ExitCode = int32(ps.ExitCode())
			}

			result.StateReset = true

			// Respawn for future commands.
			if respawnErr := s.spawn(); respawnErr != nil {
				err = fmt.Errorf("shell died and respawn failed: %w", respawnErr)
				return
			}
			return

		case <-ctx.Done():
			// Timeout: deliver remaining output before cleanup.
			s.deliverNewOutput(prefix, &stdoutOffset, &stderrOffset, onOutput)

			stdoutBytes, _ := os.ReadFile(prefix + ".stdout")
			stderrBytes, _ := os.ReadFile(prefix + ".stderr")
			result.Stdout = string(stdoutBytes)
			result.Stderr = string(stderrBytes)
			s.cleanupFiles(prefix)

			result.StateReset = true

			if respawnErr := s.killAndRespawn(); respawnErr != nil {
				err = fmt.Errorf("respawn after timeout failed: %w", respawnErr)
				return
			}
			err = context.DeadlineExceeded
			return

		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Read results.
	stdoutBytes, _ := os.ReadFile(prefix + ".stdout")
	stderrBytes, _ := os.ReadFile(prefix + ".stderr")
	statusBytes, _ := os.ReadFile(statusPath)
	cwdBytes, _ := os.ReadFile(prefix + ".pwd")

	result.Stdout = string(stdoutBytes)
	result.Stderr = string(stderrBytes)
	result.Cwd = strings.TrimSpace(string(cwdBytes))

	code, _ := strconv.ParseInt(strings.TrimSpace(string(statusBytes)), 10, 32)
	result.ExitCode = int32(code)

	s.cleanupFiles(prefix)
	return
}

// deliverNewOutput reads any new bytes from the stdout/stderr files since the
// last call and delivers them via the callback.
func (s *shellSession) deliverNewOutput(prefix string, stdoutOffset, stderrOffset *int64, onOutput OutputCallback) {
	if onOutput == nil {
		return
	}

	var newStdout, newStderr string

	if f, err := os.Open(prefix + ".stdout"); err == nil {
		if data, err := readFrom(f, *stdoutOffset); err == nil && len(data) > 0 {
			newStdout = string(data)
			*stdoutOffset += int64(len(data))
		}
		f.Close()
	}

	if f, err := os.Open(prefix + ".stderr"); err == nil {
		if data, err := readFrom(f, *stderrOffset); err == nil && len(data) > 0 {
			newStderr = string(data)
			*stderrOffset += int64(len(data))
		}
		f.Close()
	}

	if newStdout != "" || newStderr != "" {
		onOutput(newStdout, newStderr)
	}
}

// readFrom reads all bytes from a file starting at offset.
func readFrom(f *os.File, offset int64) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() <= offset {
		return nil, nil
	}
	buf := make([]byte, info.Size()-offset)
	n, err := f.ReadAt(buf, offset)
	return buf[:n], err
}

func (s *shellSession) cleanupFiles(prefix string) {
	os.Remove(prefix + ".cmd")
	os.Remove(prefix + ".stdout")
	os.Remove(prefix + ".stderr")
	os.Remove(prefix + ".status")
	os.Remove(prefix + ".pwd")
}

// Close terminates the shell session and cleans up resources.
func (s *shellSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stdin != nil {
		s.stdin.Close()
	}
	s.killProcess()
	if s.waitCh != nil {
		<-s.waitCh
	}
	if s.tempDir != "" {
		os.RemoveAll(s.tempDir)
	}
	return nil
}

// shellQuotePath wraps a path in single quotes for use in shell commands.
func shellQuotePath(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
