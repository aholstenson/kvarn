package queue_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/cmd/queue"
	"github.com/aholstenson/kvarn/internal/orchestrator"
	"github.com/aholstenson/kvarn/internal/session"
)

// serve stands up an in-process orchestrator whose backlog stays put: with no
// project store there is nothing to dispatch a job against, so the entries
// these specs write remain pending while they assert on them.
func serve(store session.Store, policy orchestrator.DispatchPolicy) string {
	svc := orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{
		SessionMgr: session.NewManager(store),
		Dispatch:   policy,
	})
	DeferCleanup(func() { svc.Shutdown(context.Background()) })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	mux := http.NewServeMux()
	path, handler := kvarnv1connect.NewOrchestratorServiceHandler(svc)
	mux.Handle(path, handler)
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	DeferCleanup(server.Close)

	return fmt.Sprintf("http://%s", listener.Addr().String())
}

func capture(fn func() error) string {
	r, w, err := os.Pipe()
	Expect(err).NotTo(HaveOccurred())
	original := os.Stdout
	os.Stdout = w

	runErr := fn()

	os.Stdout = original
	Expect(w.Close()).To(Succeed())
	out, err := io.ReadAll(r)
	Expect(err).NotTo(HaveOccurred())
	Expect(runErr).NotTo(HaveOccurred())
	return string(out)
}

var _ = Describe("kvarn queue", func() {
	var (
		ctx   context.Context
		store session.Store
		base  time.Time
	)

	BeforeEach(func() {
		ctx = context.Background()
		store = session.NewMemStore()
		base = time.Now().Add(-time.Hour)
	})

	queued := func(id, project string, priority int, at time.Time) {
		Expect(store.CreateSession(ctx, &session.Session{
			ID: id, ProjectName: project, Mode: "review", Prompt: "do " + id,
			State: session.StatePending, Priority: priority,
			CreatedAt: at, UpdatedAt: at, QueuedAt: at,
		})).To(Succeed())
	}

	It("lists the backlog in dispatch order with positions", func() {
		queued("low", "alpha", 0, base)
		queued("high", "alpha", 5, base.Add(time.Minute))

		cmd := &queue.ListCmd{}
		cmd.Addr = serve(store, orchestrator.DispatchPolicy{})

		out := capture(cmd.Run)
		Expect(out).To(ContainSubstring("EFFECTIVE"))
		Expect(out).To(MatchRegexp(`(?s)high.*low`))
	})

	It("says the backlog is empty rather than printing a bare header", func() {
		cmd := &queue.ListCmd{}
		cmd.Addr = serve(store, orchestrator.DispatchPolicy{})

		Expect(capture(cmd.Run)).To(ContainSubstring("Backlog is empty"))
	})

	It("reports depth against the configured bounds", func() {
		queued("a1", "alpha", 0, base)
		queued("b1", "beta", 0, base)

		cmd := &queue.StatusCmd{}
		cmd.Addr = serve(store, orchestrator.DispatchPolicy{MaxBacklog: 500, MaxDispatched: 8})

		out := capture(cmd.Run)
		Expect(out).To(ContainSubstring("Backlog:"))
		Expect(out).To(ContainSubstring("2 / 500"))
		Expect(out).To(ContainSubstring("0 / 8"))
		// A service with no scheduler has no pool to report against.
		Expect(out).To(ContainSubstring("unbounded (no admission control)"))
		Expect(out).To(ContainSubstring("alpha"))
		Expect(out).To(ContainSubstring("beta"))
	})

	It("says so rather than printing 0 when a bound is unset", func() {
		queued("a1", "alpha", 0, base)

		cmd := &queue.StatusCmd{}
		cmd.Addr = serve(store, orchestrator.DispatchPolicy{})

		Expect(capture(cmd.Run)).To(ContainSubstring("1 (unbounded)"))
	})

	It("emits queue stats as JSON", func() {
		queued("a1", "alpha", 0, base)

		cmd := &queue.StatusCmd{JSON: true}
		cmd.Addr = serve(store, orchestrator.DispatchPolicy{MaxBacklog: 500})

		var decoded map[string]any
		Expect(json.Unmarshal([]byte(capture(cmd.Run)), &decoded)).To(Succeed())
		Expect(decoded["backlog"]).To(BeNumerically("==", 1))
		Expect(decoded["max_backlog"]).To(BeNumerically("==", 500))
	})
})
