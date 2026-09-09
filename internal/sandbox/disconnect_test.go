package sandbox_test

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/dispatch"
	"github.com/aholstenson/kvarn/internal/sandbox"
)

var _ = Describe("BridgeProxy when the runner disconnects", func() {
	var (
		proxy     *sandbox.BridgeProxy
		pr        *dispatch.PendingRunner
		commandCh chan *v1.RunnerCommand
		resultCh  chan *v1.CommandResult
		outputCh  chan *v1.OutputChunk
		ctx       context.Context
	)

	BeforeEach(func() {
		commandCh = make(chan *v1.RunnerCommand, 1)
		resultCh = make(chan *v1.CommandResult, 1)
		outputCh = make(chan *v1.OutputChunk, 64)
		pr = &dispatch.PendingRunner{
			CommandCh: commandCh,
			ResultCh:  resultCh,
			OutputCh:  outputCh,
			DoneCh:    make(chan struct{}),
		}
		proxy = sandbox.NewBridgeProxy(commandCh, resultCh, outputCh, pr)
		pr.MarkConnected()
		ctx = context.Background()
	})

	// A command that has been delivered but can never be answered is the case
	// that used to cost the caller its whole deadline.
	It("fails a command waiting for a result", func() {
		errCh := make(chan error, 1)
		go func() {
			_, err := proxy.Exec(ctx, &v1.ExecRequest{Command: "echo"})
			errCh <- err
		}()

		Eventually(commandCh).Should(Receive())
		pr.MarkDisconnected()

		var err error
		Eventually(errCh, time.Second).Should(Receive(&err))
		Expect(err).To(MatchError(sandbox.ErrRunnerDisconnected))
	})

	// Nothing drains CommandCh once the stream is gone, so a caller can be
	// stuck before its command has been delivered at all.
	It("fails a command that is still waiting to be sent", func() {
		commandCh <- &v1.RunnerCommand{CommandId: "occupied"}

		errCh := make(chan error, 1)
		go func() {
			_, err := proxy.Exec(ctx, &v1.ExecRequest{Command: "echo"})
			errCh <- err
		}()

		pr.MarkDisconnected()

		var err error
		Eventually(errCh, time.Second).Should(Receive(&err))
		Expect(err).To(MatchError(sandbox.ErrRunnerDisconnected))
	})

	// Every command issued after the runner dies is as unanswerable as the one
	// that was in flight when it did, and must not have to discover that by
	// waiting.
	It("fails a command issued after the disconnect", func() {
		pr.MarkDisconnected()

		_, err := proxy.Exec(ctx, &v1.ExecRequest{Command: "echo"})
		Expect(err).To(MatchError(sandbox.ErrRunnerDisconnected))
	})

	It("fails a session exec", func() {
		errCh := make(chan error, 1)
		go func() {
			_, err := proxy.SessionExec(ctx, &v1.SessionExecRequest{
				SessionId: "s1",
				Command:   "sleep 60",
			}, nil)
			errCh <- err
		}()

		Eventually(commandCh).Should(Receive())
		pr.MarkDisconnected()

		var err error
		Eventually(errCh, time.Second).Should(Receive(&err))
		Expect(err).To(MatchError(sandbox.ErrRunnerDisconnected))
	})

	It("reports what the guest printed before it went quiet", func() {
		console := sandbox.NewConsoleTail()
		console.Add("Out of memory: Killed process 412 (kvarn)\n")
		proxy.AttachConsole(console)

		errCh := make(chan error, 1)
		go func() {
			_, err := proxy.Exec(ctx, &v1.ExecRequest{Command: "echo"})
			errCh <- err
		}()

		Eventually(commandCh).Should(Receive())
		pr.MarkDisconnected()

		var err error
		Eventually(errCh, time.Second).Should(Receive(&err))
		Expect(err).To(MatchError(sandbox.ErrRunnerDisconnected))
		Expect(err.Error()).To(ContainSubstring("Killed process 412"))
	})

	// A runner systemd restarts takes the token again, and the commands sent to
	// it must wait rather than inherit the previous stream's failure.
	It("waits again once a replacement runner registers", func() {
		pr.MarkDisconnected()
		pr.MarkConnected()

		errCh := make(chan error, 1)
		go func() {
			_, err := proxy.Exec(ctx, &v1.ExecRequest{Command: "echo"})
			errCh <- err
		}()

		var cmd *v1.RunnerCommand
		Eventually(commandCh).Should(Receive(&cmd))
		Consistently(errCh, 100*time.Millisecond).ShouldNot(Receive())

		resultCh <- &v1.CommandResult{
			CommandId: cmd.CommandId,
			Result: &v1.CommandResult_Exec{
				Exec: &v1.ExecResponse{ExitCode: 0, Stdout: "hello\n"},
			},
		}

		var err error
		Eventually(errCh, time.Second).Should(Receive(&err))
		Expect(err).NotTo(HaveOccurred())
	})

	// Before any runner has attached there is no stream to lose, so the caller
	// stays bounded by its own deadline rather than failing on a signal that
	// would mean nothing.
	It("leaves a caller on its own deadline when no runner has ever connected", func() {
		fresh := &dispatch.PendingRunner{
			CommandCh: commandCh,
			ResultCh:  resultCh,
			OutputCh:  outputCh,
			DoneCh:    make(chan struct{}),
		}
		p := sandbox.NewBridgeProxy(commandCh, resultCh, outputCh, fresh)

		deadlineCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()

		_, err := p.Exec(deadlineCtx, &v1.ExecRequest{Command: "echo"})
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, sandbox.ErrRunnerDisconnected)).To(BeFalse())
		Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue())
	})
})
