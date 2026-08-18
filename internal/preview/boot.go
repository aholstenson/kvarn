package preview

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox"
)

// Bringing a preview up is the same two steps wherever it happens: start the
// declared servers as long-lived guest processes, then hold until the ready
// checks pass. The orchestrator wraps that in cloning, hostnames and durable
// state; `kvarn local preview` wraps it in the working tree and a port forward.
// Both call the functions here, so a preview that works on a developer's
// machine is running the same thing the operator's host will run.

// ReadyAttempts and ReadyInterval bound how long the ready checks are retried.
// A server takes a few seconds to bind its port after the process starts, so a
// single attempt would fail a preview that is merely still starting; a couple
// of minutes is generous for that and short enough that a genuinely broken
// preview reports itself rather than hanging.
const (
	ReadyAttempts = 40
	ReadyInterval = 3 * time.Second
)

// GuestCallTimeout bounds how long each individual guest call during a boot may
// take. The boot as a whole is unbounded on purpose — a cold nixpkgs install is
// genuinely slow — but no single RPC should be.
const GuestCallTimeout = 2 * time.Minute

// ServeOpts is everything StartServices needs that does not come from the
// repository's own configuration.
type ServeOpts struct {
	// WorkspaceDir is the guest directory the serve commands run in. A step's
	// own relative working_dir is resolved under it.
	WorkspaceDir string
	// URLs maps site name to the address that site answers on. Every serve step
	// gets all of them in its environment: a server that cannot emit correct
	// absolute URLs — for its own assets, for OAuth redirects — is the most
	// common way a preview ends up half-broken, and one serving several sites
	// needs all of their names to tell its virtual hosts apart.
	URLs map[string]string
	// IDPrefix namespaces the guest process IDs, so two previews sharing a
	// runner cannot collide on the step's position alone.
	IDPrefix string

	// OnStarting is called with a serve step's name just before it is started.
	OnStarting func(name string)
	// OnOutput receives everything a serve step writes.
	OnOutput func(name, stdout, stderr string)
	// OnExit fires once per serve step that ends, whether it was stopped or
	// died on its own.
	OnExit func(name string, exitCode int32, err error)
}

// StartServices starts every serve step the repository declares as a long-lived
// process in the guest. It returns once they are all running; whether they stay
// up is what the ready checks decide.
func StartServices(ctx context.Context, procs sandbox.ProcessRunner, cfg *project.Config, opts ServeOpts) error {
	if procs == nil {
		return errors.New("this sandbox cannot run long-lived processes")
	}

	env := make(map[string]string, len(opts.URLs))
	for name, url := range opts.URLs {
		env[project.EnvVarName(name)] = url
	}

	for i, proc := range cfg.Preview.Serve {
		workingDir := opts.WorkspaceDir
		if proc.WorkingDir != "" {
			workingDir = workingDir + "/" + proc.WorkingDir
		}

		name := proc.Name
		if opts.OnStarting != nil {
			opts.OnStarting(name)
		}

		startCtx, cancel := context.WithTimeout(ctx, GuestCallTimeout)
		_, err := procs.StartProcess(startCtx, &v1.StartProcessRequest{
			ProcessId:  fmt.Sprintf("%s/serve-%d", opts.IDPrefix, i),
			Name:       name,
			Command:    proc.Run,
			WorkingDir: workingDir,
			Env:        env,
		}, func(stdout, stderr string) {
			if opts.OnOutput != nil {
				opts.OnOutput(name, stdout, stderr)
			}
		}, func(exitCode int32, exitErr error) {
			if opts.OnExit != nil {
				opts.OnExit(name, exitCode, exitErr)
			}
		})
		cancel()
		if err != nil {
			return fmt.Errorf("start serve step %q: %w", name, err)
		}
	}
	return nil
}

// WaitReady runs the ready checks in order until each passes, retrying while
// the servers finish binding their ports. onPassed, when set, is called with
// each check's name as it goes green, so a caller can report progress on a wait
// that legitimately takes minutes.
func WaitReady(
	ctx context.Context,
	runner sandbox.RunnerProxy,
	shellSessionID string,
	steps []project.Step,
	onPassed func(name string),
) error {
	for _, step := range steps {
		command := step.Run
		if step.WorkingDir != "" {
			command = fmt.Sprintf("cd %s && %s", ShellQuote(step.WorkingDir), step.Run)
		}

		var lastErr error
		passed := false
		for attempt := range ReadyAttempts {
			if err := ctx.Err(); err != nil {
				return err
			}
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(ReadyInterval):
				}
			}

			execCtx, cancel := context.WithTimeout(ctx, GuestCallTimeout)
			resp, err := runner.SessionExec(execCtx, &v1.SessionExecRequest{
				SessionId:      shellSessionID,
				Command:        command,
				TimeoutSeconds: step.Timeout.Seconds(),
			}, nil)
			cancel()

			switch {
			case err != nil:
				lastErr = err
			case resp.ExitCode != 0:
				lastErr = fmt.Errorf("exit code %s: %s", FormatExitCode(resp.ExitCode), strings.TrimSpace(resp.Stderr))
			default:
				passed = true
			}
			if passed {
				break
			}
		}
		if !passed {
			return fmt.Errorf("ready check %q never passed after %s: %w",
				step.Name, time.Duration(ReadyAttempts)*ReadyInterval, lastErr)
		}
		if onPassed != nil {
			onPassed(step.Name)
		}
	}
	return nil
}

// ShellQuote wraps a path for safe interpolation into a shell command.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// FormatExitCode renders a process exit status, naming the signal for one a
// signal ended rather than leaving the reader to decode 128+n.
func FormatExitCode(code int32) string {
	if code > 128 && code < 192 {
		return fmt.Sprintf("%d (signal %d)", code, code-128)
	}
	return fmt.Sprintf("%d", code)
}
