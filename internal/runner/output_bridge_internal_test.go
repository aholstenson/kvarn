package runner

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/dispatch"
	"github.com/aholstenson/kvarn/internal/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// A command's output crosses the bridge in protobuf strings, which refuse
// bytes that are not UTF-8. A command that prints such bytes must still be
// answered, and the connection must survive answering it: a runner that goes
// away over one command's output takes every later command with it, because
// nothing can bring it back with the token it registered under. This runs
// through the real bridge and command loop because the report that fails is
// the loop's, not any handler's.
var _ = Describe("output that is not UTF-8", func() {
	var (
		bridge   *http.Server
		listener net.Listener
		proxy    *sandbox.BridgeProxy
		pr       *dispatch.PendingRunner
		cancel   context.CancelFunc
		ctx      context.Context
	)

	BeforeEach(func() {
		registry := dispatch.NewRegistry()
		var err error
		pr, err = registry.Register("utf8-token")
		Expect(err).NotTo(HaveOccurred())

		listener, err = net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())

		mux := http.NewServeMux()
		path, h := kvarnv1connect.NewBridgeServiceHandler(dispatch.NewHandler(registry))
		mux.Handle(path, h)
		bridge = &http.Server{Handler: h2c.NewHandler(mux, &http2.Server{})}
		go bridge.Serve(listener)

		ctx, cancel = context.WithCancel(context.Background())
		go connectToOrchestrator(ctx, http.DefaultClient,
			fmt.Sprintf("http://%s", listener.Addr().String()), "utf8-token")

		Eventually(pr.DoneCh, "5s").Should(BeClosed())
		proxy = sandbox.NewBridgeProxy(pr.CommandCh, pr.ResultCh, pr.OutputCh, pr)
	})

	AfterEach(func() {
		cancel()
		bridge.Close()
	})

	It("is delivered with the bytes replaced, and the runner stays connected", func() {
		sess, err := proxy.CreateSession(ctx, &v1.CreateSessionRequest{})
		Expect(err).NotTo(HaveOccurred())

		callCtx, callCancel := context.WithTimeout(ctx, 10*time.Second)
		defer callCancel()

		var streamed string
		resp, err := proxy.SessionExec(callCtx, &v1.SessionExecRequest{
			SessionId: sess.SessionId,
			Command:   `printf 'value: \261 end\n'; printf 'err: \377\n' >&2`,
		}, func(stdout, stderr string) {
			streamed += stdout + stderr
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.ExitCode).To(Equal(int32(0)))
		Expect(resp.Stdout).To(Equal("value: � end\n"))
		Expect(resp.Stderr).To(Equal("err: �\n"))
		Expect(streamed).To(ContainSubstring("value: � end"))

		// The connection is still the same one, and it still answers.
		Consistently(pr.Disconnected(), "200ms").ShouldNot(BeClosed())
		again, err := proxy.SessionExec(callCtx, &v1.SessionExecRequest{
			SessionId: sess.SessionId,
			Command:   "echo still here",
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(again.Stdout).To(Equal("still here\n"))
	})
})
