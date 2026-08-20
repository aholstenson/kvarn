package runner

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

// A caller that issues several commands at once gives each its own deadline,
// and expects each to be judged on its own work. Running them one at a time in
// the guest breaks that: the cheap ones spend their whole budget waiting for
// the expensive one and expire having never run. This is exercised through the
// real bridge and the real command loop, because the serialization it guards
// against lives in the loop rather than in any one handler.
var _ = Describe("concurrent commands", func() {
	var (
		bridge   *http.Server
		listener net.Listener
		proxy    *sandbox.BridgeProxy
		cancel   context.CancelFunc
		ctx      context.Context
	)

	BeforeEach(func() {
		registry := dispatch.NewRegistry()
		pr, err := registry.Register("concurrency-token")
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
			fmt.Sprintf("http://%s", listener.Addr().String()), "concurrency-token")

		Eventually(pr.DoneCh, "5s").Should(BeClosed())
		proxy = sandbox.NewBridgeProxy(pr.CommandCh, pr.ResultCh, pr.OutputCh, pr)
	})

	AfterEach(func() {
		cancel()
		bridge.Close()
	})

	It("answers a quick command while a slow one is still running", func() {
		sess, err := proxy.CreateSession(ctx, &v1.CreateSessionRequest{})
		Expect(err).NotTo(HaveOccurred())

		// The shell may run as a different user than the test, so the marker it
		// writes goes somewhere both can reach.
		dir, err := os.MkdirTemp("", "kvarn-concurrency-*")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { os.RemoveAll(dir) })
		Expect(os.Chmod(dir, 0o777)).To(Succeed())
		marker := filepath.Join(dir, "started")

		slowDone := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(slowDone)
			_, execErr := proxy.SessionExec(ctx, &v1.SessionExecRequest{
				SessionId: sess.SessionId,
				Command:   fmt.Sprintf("touch %s; sleep 5", marker),
			}, nil)
			Expect(execErr).NotTo(HaveOccurred())
		}()

		// Wait for the slow command to be underway, so what follows is genuinely
		// contending with it rather than racing it to the runner.
		Eventually(func() bool {
			_, statErr := os.Stat(marker)
			return statErr == nil
		}, "10s").Should(BeTrue())

		started := time.Now()
		resp, err := proxy.Exec(ctx, &v1.ExecRequest{Command: "echo quick"})
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Stdout).To(ContainSubstring("quick"))
		Expect(time.Since(started)).To(BeNumerically("<", 3*time.Second),
			"the quick command waited for the slow one to finish")

		Eventually(slowDone, "15s").Should(BeClosed())
	})
})
