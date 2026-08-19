package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/preview/snapshot"
	"github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox"
)

// Taking a preview down is the same sequence wherever it happens, for the same
// reason bringing one up is: a preview that kept its data on the operator's
// host and lost it on a developer's machine would be a preview nobody trusts.
// So the quiesce-tar-stream side lives here beside boot.go, and the
// orchestrator and `kvarn local preview` each supply only what they know — the
// sandbox, the archive store, and how long they are willing to wait.

// stateTmpDir is where the archive is staged inside the guest, matching where
// the cache transfer stages its tarballs. /tmp is restricted in some VM
// configurations, /var/tmp is not.
const stateTmpDir = "/var/tmp/kvarn-preview-state"

// stateTmpFile is the staged archive itself. One per VM is enough: a preview
// has exactly one state archive and nothing else writes here.
const stateTmpFile = stateTmpDir + "/state.tar.zst"

// nothingToCapture is the exit code the capture script uses to say the guest
// held none of the declared paths. It is distinct from a tar failure so an
// empty preview is reported as empty rather than as broken.
const nothingToCapture = 3

// DefaultStopGrace is how long a preview's servers are given to shut down after
// SIGTERM before they are killed. A database that flushes on SIGTERM is the
// case this exists for, and ten seconds is long enough for one without making a
// drain wait on a server that is never going to exit.
const DefaultStopGrace = 10 * time.Second

// Env is the environment every part of a preview sees: one variable per site
// carrying its URL, plus the state directory. Building it in one place is what
// keeps the shell session, the VM-wide environment and each serve process's own
// environment from drifting apart.
func Env(urls map[string]string) map[string]string {
	env := make(map[string]string, len(urls)+1)
	for name, url := range urls {
		env[project.EnvVarName(name)] = url
	}
	env[project.EnvVarStateDir] = project.GuestPreviewState
	return env
}

// PrepareStateDir creates the preview's state directory and gives it to the
// kvarn user. It runs on every boot, whether or not the repository declares any
// state: $KVARN_PREVIEW_STATE_DIR has to name a directory that exists before a
// setup step can write into it.
func PrepareStateDir(ctx context.Context, proxy sandbox.RunnerProxy) error {
	dir := project.GuestPreviewState
	script := fmt.Sprintf("mkdir -p %s && chown kvarn:kvarn %s",
		ShellQuote(dir), strings.Join(quoteAll(sandbox.OwnedDirs(dir)), " "))
	return runPrivileged(ctx, proxy, "prepare preview state directory", script)
}

// HasState reports whether the guest holds anything worth capturing: an entry
// in the state directory, or one of the declared paths. It is one cheap call,
// and it is what keeps a stateless preview tearing down exactly as fast as it
// did before any of this existed.
func HasState(ctx context.Context, proxy sandbox.RunnerProxy, st project.PreviewState) (bool, error) {
	var checks strings.Builder
	fmt.Fprintf(&checks, "if [ -d %s ] && [ -n \"$(ls -A %s 2>/dev/null)\" ]; then exit 0; fi\n",
		ShellQuote(project.GuestPreviewState), ShellQuote(project.GuestPreviewState))
	for _, p := range st.Paths {
		fmt.Fprintf(&checks, "if [ -e %s ]; then exit 0; fi\n", ShellQuote(p))
	}
	checks.WriteString("exit 1\n")

	execCtx, cancel := context.WithTimeout(ctx, GuestCallTimeout)
	defer cancel()
	resp, err := proxy.Exec(execCtx, &v1.ExecRequest{
		Command:    "sh",
		Args:       []string{"-c", checks.String()},
		Privileged: true,
	})
	if err != nil {
		return false, fmt.Errorf("check for preview state: %w", err)
	}
	return resp.ExitCode == 0, nil
}

// RestoreOpts is everything Restore needs.
type RestoreOpts struct {
	// Proxy is the bare proxy into the VM. The transfer and the privileged
	// extract have to reach the machine itself, not whatever container a step
	// wrapped itself in.
	Proxy sandbox.RunnerProxy
	// Runner and ShellSessionID are where the repository's restore steps run,
	// so they see the same environment the setup steps will.
	Runner         sandbox.RunnerProxy
	ShellSessionID string

	Store snapshot.Store
	ID    snapshot.ID
	State project.PreviewState

	// OnStep and OnOutput report the restore steps as they run.
	OnStep   func(name string)
	OnOutput func(name, stdout, stderr string)
}

// Restore unpacks a preview's stored state into a freshly booted guest and runs
// the repository's restore steps. It reports false when there is nothing
// stored, which is the ordinary first boot of every preview.
//
// A failure here fails the boot. A preview that comes up empty after somebody
// spent an afternoon entering data into it is worse than one that refuses to
// come up and says why — and there are two escapes from a snapshot that cannot
// be restored, `--fresh` and `preview reset`.
func Restore(ctx context.Context, opts RestoreOpts) (bool, error) {
	archive, meta, err := opts.Store.Open(opts.ID)
	if errors.Is(err, snapshot.ErrNoSnapshot) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open stored preview state: %w", err)
	}
	defer archive.Close()

	if err := runPrivileged(ctx, opts.Proxy, "prepare preview state transfer",
		fmt.Sprintf("mkdir -p %s", ShellQuote(stateTmpDir))); err != nil {
		return false, err
	}

	streamCtx, cancel := context.WithTimeout(ctx, GuestCallTimeout)
	err = opts.Proxy.StreamToGuest(streamCtx, stateTmpFile, archive, 0)
	cancel()
	if err != nil {
		return false, fmt.Errorf("stream preview state into the guest: %w", err)
	}

	// The archive holds absolute paths with their original ownership, so it is
	// unpacked from / as root. What the privileged mkdir -p created on the way
	// there is what needs handing back to the kvarn user.
	var chown []string
	for _, dir := range append([]string{project.GuestPreviewState}, opts.State.Paths...) {
		chown = append(chown, sandbox.OwnedDirs(dir)...)
	}
	script := fmt.Sprintf("set -e\nmkdir -p %s\nchown kvarn:kvarn %s\ntar -C / --zstd -xf %s\nrm -f %s\n",
		strings.Join(quoteAll(chown), " "), strings.Join(quoteAll(chown), " "),
		ShellQuote(stateTmpFile), ShellQuote(stateTmpFile))
	if err := runPrivileged(ctx, opts.Proxy, "unpack preview state", script); err != nil {
		return false, err
	}

	// Restoring is using it, and use is what holds the prune horizon away. A
	// preview somebody comes back to every few days must not be swept for
	// having been captured a month ago.
	if err := opts.Store.Touch(opts.ID); err != nil {
		slog.Warn("could not stamp restored preview state", "error", err)
	}

	slog.Info("restored preview state",
		"bytes", meta.Bytes, "captured_at", meta.CreatedAt, "commit", meta.Commit)

	if len(opts.State.Restore) > 0 {
		if err := runStepList(ctx, opts.Runner, opts.ShellSessionID, "state restore step",
			opts.State.Restore, opts.OnStep, opts.OnOutput, nil); err != nil {
			return false, err
		}
	}
	return true, nil
}

// CaptureOpts is everything Capture needs.
type CaptureOpts struct {
	// Proxy is the bare proxy into the VM; Runner and ShellSessionID are where
	// the repository's save steps run, with the site URLs still exported.
	Proxy          sandbox.RunnerProxy
	Runner         sandbox.RunnerProxy
	ShellSessionID string

	Store snapshot.Store
	ID    snapshot.ID
	State project.PreviewState

	// MaxBytes caps the archive. Zero leaves it uncapped. It is enforced on the
	// way past on the host, so an over-cap capture fails before anything is
	// renamed into place and costs no second call into the guest.
	MaxBytes int64
	// Meta describes what is being captured: the commit that produced it and
	// the hostnames it answered on. Size and time are filled in by the store.
	Meta snapshot.Meta

	OnStep   func(name string)
	OnOutput func(name, stdout, stderr string)
}

// Capture quiesces a preview and writes its declared state to the host.
//
// It runs while the VM is still up and after its servers have been stopped, so
// what reaches the archive is a database that was shut down rather than one
// that was photographed mid-write. The archive it produces replaces the
// preview's current one and rotates that to the previous generation.
func Capture(ctx context.Context, opts CaptureOpts) (snapshot.Meta, error) {
	if len(opts.State.Save) > 0 {
		if err := runStepList(ctx, opts.Runner, opts.ShellSessionID, "state save step",
			opts.State.Save, opts.OnStep, opts.OnOutput, nil); err != nil {
			return snapshot.Meta{}, err
		}
	}

	// tar is given paths relative to /, so one archive can hold the state
	// directory and declared paths from anywhere else in the filesystem and
	// unpack them all back where they came from.
	rel := make([]string, 0, len(opts.State.Paths)+1)
	for _, p := range append([]string{project.GuestPreviewState}, opts.State.Paths...) {
		rel = append(rel, strings.TrimPrefix(p, "/"))
	}
	script := fmt.Sprintf(`set -e
mkdir -p %s
rm -f %s
set --
for p in %s; do
  if [ -e "/$p" ]; then set -- "$@" "$p"; fi
done
if [ "$#" -eq 0 ]; then exit %d; fi
tar -C / --zstd -cf %s "$@"
`, ShellQuote(stateTmpDir), ShellQuote(stateTmpFile),
		strings.Join(quoteAll(rel), " "), nothingToCapture, ShellQuote(stateTmpFile))

	execCtx, cancel := context.WithTimeout(ctx, GuestCallTimeout)
	resp, err := opts.Proxy.Exec(execCtx, &v1.ExecRequest{
		Command:    "sh",
		Args:       []string{"-c", script},
		Privileged: true,
	})
	cancel()
	if err != nil {
		return snapshot.Meta{}, fmt.Errorf("archive preview state: %w", err)
	}
	if resp.ExitCode == nothingToCapture {
		return snapshot.Meta{}, nil
	}
	if resp.ExitCode != 0 {
		return snapshot.Meta{}, fmt.Errorf("archive preview state: exit code %s: %s",
			FormatExitCode(resp.ExitCode), strings.TrimSpace(resp.Stderr))
	}

	meta := opts.Meta
	meta.CreatedAt = time.Time{} // the store stamps it, so one clock decides

	pr, pw := io.Pipe()
	saveErr := make(chan error, 1)
	go func() {
		err := opts.Store.Save(opts.ID, meta, &cappedReader{r: pr, max: opts.MaxBytes})
		// Stop the guest pushing the rest of an archive nobody is going to keep.
		pr.CloseWithError(err)
		saveErr <- err
	}()

	streamCtx, cancel := context.WithTimeout(ctx, GuestCallTimeout)
	streamErr := opts.Proxy.StreamFromGuest(streamCtx, stateTmpFile, pw)
	cancel()
	pw.Close()

	if err := <-saveErr; err != nil {
		cleanupStateTmp(ctx, opts.Proxy)
		return snapshot.Meta{}, fmt.Errorf("store preview state: %w", err)
	}
	if streamErr != nil {
		cleanupStateTmp(ctx, opts.Proxy)
		return snapshot.Meta{}, fmt.Errorf("stream preview state out of the guest: %w", streamErr)
	}
	cleanupStateTmp(ctx, opts.Proxy)

	stored, err := opts.Store.Stat(opts.ID)
	if err != nil {
		return snapshot.Meta{}, fmt.Errorf("stat stored preview state: %w", err)
	}
	return stored, nil
}

// StopServices shuts down the preview's serve processes, in reverse of the
// order they were started, before its state is captured.
//
// Without this the VM is destroyed out from under the servers, which is fine
// for a preview that holds nothing and is exactly how a database ends up
// captured mid-write. It runs only on the capture path, so a stateless preview
// still tears down in one call.
func StopServices(
	ctx context.Context,
	procs sandbox.ProcessRunner,
	cfg *project.Config,
	idPrefix string,
	grace time.Duration,
) error {
	if procs == nil || len(cfg.Preview.Serve) == 0 {
		return nil
	}
	if grace <= 0 {
		grace = DefaultStopGrace
	}

	var errs []error
	for i := len(cfg.Preview.Serve) - 1; i >= 0; i-- {
		proc := cfg.Preview.Serve[i]
		stopCtx, cancel := context.WithTimeout(ctx, GuestCallTimeout)
		_, err := procs.StopProcess(stopCtx, &v1.StopProcessRequest{
			ProcessId:    fmt.Sprintf("%s/serve-%d", idPrefix, i),
			GraceSeconds: uint32(grace.Seconds()),
		})
		cancel()
		switch {
		case err == nil:
		case connect.CodeOf(err) == connect.CodeNotFound:
			// The process died on its own hours ago. Nothing to stop, and
			// nothing worth reporting to the operator about the capture.
			slog.Debug("preview serve step was already gone", "step", proc.Name)
		default:
			errs = append(errs, fmt.Errorf("stop serve step %q: %w", proc.Name, err))
		}
	}
	return errors.Join(errs...)
}

// cleanupStateTmp removes the staged archive. Best-effort: the VM is about to
// be destroyed anyway, and on the restore path the space matters more than the
// error would.
func cleanupStateTmp(ctx context.Context, proxy sandbox.RunnerProxy) {
	execCtx, cancel := context.WithTimeout(ctx, GuestCallTimeout)
	defer cancel()
	_, _ = proxy.Exec(execCtx, &v1.ExecRequest{
		Command:    "rm",
		Args:       []string{"-f", stateTmpFile},
		Privileged: true,
	})
}

// runPrivileged runs one shell script in the guest as root and turns a non-zero
// exit into an error naming what was being attempted.
func runPrivileged(ctx context.Context, proxy sandbox.RunnerProxy, what, script string) error {
	execCtx, cancel := context.WithTimeout(ctx, GuestCallTimeout)
	defer cancel()
	resp, err := proxy.Exec(execCtx, &v1.ExecRequest{
		Command:    "sh",
		Args:       []string{"-c", script},
		Privileged: true,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("%s: exit code %s: %s", what,
			FormatExitCode(resp.ExitCode), strings.TrimSpace(resp.Stderr))
	}
	return nil
}

// quoteAll shell-quotes a list of paths for interpolation into a script.
func quoteAll(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, ShellQuote(p))
	}
	sort.Strings(out)
	return out
}

// ErrStateTooLarge is what a capture over its cap fails with. It is its own
// error so the reason reaching the operator names the cap rather than an I/O
// failure part-way through a copy.
var ErrStateTooLarge = errors.New("preview state exceeds its maximum size")

// cappedReader fails once more than max bytes have crossed it. It sits between
// the guest stream and the store, so the cap is enforced before anything is
// renamed into place: an over-cap capture costs a truncated temp file and
// leaves the preview's previous archive exactly where it was.
type cappedReader struct {
	r    io.Reader
	max  int64
	read int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.max > 0 && c.read > c.max {
		return n, fmt.Errorf("%w of %d bytes", ErrStateTooLarge, c.max)
	}
	return n, err
}
