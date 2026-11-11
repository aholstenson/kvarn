package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aholstenson/kvarn/internal/dispatch"
	"github.com/aholstenson/kvarn/internal/vm"
)

// bootRetryBackoff is the pause before each new boot attempt, so its length is
// also the number of retries. A boot fails for reasons that belong to that one
// VM: the hypervisor does not start it, or the guest never gets as far as
// starting the runner. A new VM usually boots where the last one did not, and
// nothing outside the host has to recover first, so the waits are short.
var bootRetryBackoff = []time.Duration{2 * time.Second, 5 * time.Second}

// runnerRegisterTimeout is how long a started VM has to bring up its runner
// and register with the bridge before the boot counts as failed.
var runnerRegisterTimeout = 30 * time.Second

// bootedVM is a VM whose runner has registered with the bridge.
type bootedVM struct {
	instance *vm.VM
	runner   *dispatch.PendingRunner

	// console holds the tail of this VM's serial console. It belongs to the
	// VM that booted, so a later report of a lost bridge never shows the
	// console of an attempt that was discarded.
	console *ConsoleTail

	// closers release what the boot acquired, in the order it was acquired.
	// The session takes them over when the boot succeeds.
	closers []func()
}

// release runs the closers in reverse order.
func (b *bootedVM) release() {
	for i := len(b.closers) - 1; i >= 0; i-- {
		runCloser(b.closers[i])
	}
	b.closers = nil
}

// bootVM creates a VM and waits for its runner to register. When either step
// fails, it destroys that VM and boots a new one, up to the attempts that
// bootRetryBackoff allows.
//
// Only the boot is retried. After the runner registers, the guest holds state
// (the transferred workspace, the installed dependencies), so a later failure
// cannot be repaired by a new VM without doing all of that work again.
func bootVM(
	ctx context.Context,
	opts Opts,
	createOpts vm.CreateOpts,
	registry *dispatch.Registry,
	handler *dispatch.Handler,
) (*bootedVM, error) {
	maxAttempts := len(bootRetryBackoff) + 1

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		booted, err := bootVMOnce(ctx, opts, createOpts, registry, handler)
		if err == nil {
			return booted, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, err
		}
		if attempt == maxAttempts {
			break
		}

		wait := bootRetryBackoff[attempt-1]
		slog.Warn("VM boot failed; retrying with a new VM",
			"error", err, "attempt", attempt, "max_attempts", maxAttempts, "wait", wait)
		// The notice goes on the console stream because that is where a viewer
		// sees the boot. Without it, a second boot log after the first one
		// looks like the guest restarted for no reason.
		emit(opts, ConsoleOutputEvent{Output: fmt.Sprintf(
			"kvarn: VM boot failed: %v; retrying with a new VM in %s (attempt %d of %d)\n",
			err, wait, attempt+1, maxAttempts)})

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: %w", ctx.Err(), lastErr)
		case <-time.After(wait):
		}
	}

	return nil, fmt.Errorf("boot VM after %s: %w", attemptCount(maxAttempts), lastErr)
}

// bootVMOnce makes one boot attempt. On failure it releases everything the
// attempt acquired, so the caller can start again from nothing.
func bootVMOnce(
	ctx context.Context,
	opts Opts,
	createOpts vm.CreateOpts,
	registry *dispatch.Registry,
	handler *dispatch.Handler,
) (_ *bootedVM, retErr error) {
	b := &bootedVM{}
	defer func() {
		if retErr != nil {
			b.release()
		}
	}()

	// Each attempt registers its own token, so a runner from a discarded VM
	// that connects late cannot take the place of the runner in the new VM.
	token, err := generateToken()
	if err != nil {
		return nil, fmt.Errorf("generate token: %w", err)
	}
	pr, err := registry.Register(token)
	if err != nil {
		return nil, fmt.Errorf("register token: %w", err)
	}
	b.closers = append(b.closers, func() { registry.Remove(token) })

	createOpts.Token = token
	// The console is the guest's only voice once the runner stops answering, so
	// it is retained as well as emitted: the tail is what a lost bridge is
	// reported with, and the debug log is where the whole stream can be read
	// back when the tail is not enough.
	b.console = NewConsoleTail()
	createOpts.OnConsoleOutput = func(output string) {
		b.console.Add(output)
		slog.Debug("guest console", "output", strings.TrimRight(output, "\n"))
		emit(opts, ConsoleOutputEvent{Output: output})
	}

	instance, runnerConn, err := opts.Provider.Create(ctx, createOpts)
	if err != nil {
		return nil, fmt.Errorf("create VM: %w", err)
	}
	b.instance = instance
	b.closers = append(b.closers, func() {
		slog.Info("destroying VM", "vm_id", instance.ID)
		destroyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if destroyErr := opts.Provider.Destroy(destroyCtx, instance.ID); destroyErr != nil {
			slog.Error("failed to destroy VM", "vm_id", instance.ID, "error", destroyErr)
		}
	})

	// Bridge is reachable only over the provider-supplied transport so that
	// nothing outside the guest can talk to it. Each provider must hand us a
	// listener (vsock today; future remote providers must design an
	// authenticated transport rather than fall back to an open TCP socket).
	if runnerConn == nil || runnerConn.Listener == nil {
		return nil, errors.New("provider did not supply a runner listener; bridge cannot be served safely")
	}
	bridgeListener := dispatch.WrapListener(runnerConn.Listener, runnerConn.ExpectedPeerCID)
	go dispatch.Serve(bridgeListener, handler)
	b.closers = append(b.closers, func() { bridgeListener.Close() })

	timer := time.NewTimer(runnerRegisterTimeout)
	defer timer.Stop()
	select {
	case <-pr.DoneCh:
	case <-ctx.Done():
		return nil, fmt.Errorf("wait for runner to connect: %w", ctx.Err())
	case <-timer.C:
		return nil, fmt.Errorf("timed out after %s waiting for runner to connect", runnerRegisterTimeout)
	}

	b.runner = pr
	return b, nil
}
