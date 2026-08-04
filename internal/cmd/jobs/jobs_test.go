package jobs_test

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
	"github.com/aholstenson/kvarn/internal/cmd/jobs"
	"github.com/aholstenson/kvarn/internal/orchestrator"
	"github.com/aholstenson/kvarn/internal/session"
)

// serve stands up an in-process orchestrator over the sessions in store and
// returns its address. These specs run the CLI commands themselves rather than
// the RPCs they call, so the rendering a user actually sees is what is
// asserted on.
func serve(store session.Store) string {
	svc := orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{
		SessionMgr: session.NewManager(store),
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

// capture runs fn with os.Stdout redirected, returning what it wrote.
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

var _ = Describe("kvarn jobs", func() {
	var (
		ctx   context.Context
		store session.Store
		addr  string
	)

	BeforeEach(func() {
		ctx = context.Background()
		store = session.NewMemStore()
		now := time.Now()

		Expect(store.CreateSession(ctx, &session.Session{
			ID: "waiting", ProjectName: "alpha", Mode: "implement",
			Prompt:    "make the tests pass",
			State:     session.StatePending,
			Priority:  3,
			CreatedAt: now.Add(-30 * time.Minute), UpdatedAt: now, QueuedAt: now.Add(-30 * time.Minute),
		})).To(Succeed())
		Expect(store.CreateSession(ctx, &session.Session{
			ID: "broken", ProjectName: "beta", Mode: "feedback", PRRef: "42",
			Prompt:    "address review comments",
			State:     session.StateFailed,
			Error:     "agent gave up",
			CreatedAt: now.Add(-10 * time.Minute), UpdatedAt: now,
		})).To(Succeed())

		addr = serve(store)
	})

	It("renders a table of jobs newest first", func() {
		cmd := &jobs.ListCmd{}
		cmd.Addr = addr

		out := capture(cmd.Run)
		Expect(out).To(ContainSubstring("SESSION"))
		Expect(out).To(ContainSubstring("broken"))
		Expect(out).To(ContainSubstring("waiting"))
		Expect(out).To(ContainSubstring("make the tests pass"))
		// Newest first: the failed job was created more recently.
		Expect(out).To(MatchRegexp(`(?s)broken.*waiting`))
	})

	It("filters server-side on state", func() {
		cmd := &jobs.ListCmd{State: []string{"pending"}}
		cmd.Addr = addr

		out := capture(cmd.Run)
		Expect(out).To(ContainSubstring("waiting"))
		Expect(out).NotTo(ContainSubstring("broken"))
	})

	It("emits a JSON array with proto field names", func() {
		cmd := &jobs.ListCmd{JSON: true, State: []string{"failed"}}
		cmd.Addr = addr

		var decoded []map[string]any
		Expect(json.Unmarshal([]byte(capture(cmd.Run)), &decoded)).To(Succeed())
		Expect(decoded).To(HaveLen(1))
		Expect(decoded[0]["session_id"]).To(Equal("broken"))
		Expect(decoded[0]["pr_ref"]).To(Equal("42"))
		// Timestamps survive the round trip rather than arriving as raw micros.
		Expect(decoded[0]["created_at"]).To(BeAssignableToTypeOf(""))
	})

	It("shows one job with its queue fields", func() {
		cmd := &jobs.ShowCmd{SessionID: "waiting"}
		cmd.Addr = addr

		out := capture(cmd.Run)
		Expect(out).To(ContainSubstring("Session:"))
		Expect(out).To(ContainSubstring("Priority:"))
		Expect(out).To(ContainSubstring("make the tests pass"))
	})

	It("cancels by filter and reports what it stopped", func() {
		cmd := &jobs.CancelCmd{Project: "alpha", Reason: "wrong branch"}
		cmd.Addr = addr

		out := capture(cmd.Run)
		Expect(out).To(ContainSubstring("waiting"))
		Expect(out).To(ContainSubstring("Cancelling 1 job(s)"))

		got, err := store.GetSession(ctx, "waiting")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.State).To(Equal(session.StateCancelled))
	})

	It("refuses a session id combined with filter flags", func() {
		cmd := &jobs.CancelCmd{SessionID: "waiting", Project: "alpha"}
		cmd.Addr = addr

		err := cmd.Run()
		Expect(err).To(MatchError(ContainSubstring("not both")))
	})
})

var _ = Describe("formatting helpers", func() {
	It("collapses a multi-line prompt to one truncated line", func() {
		Expect(jobs.Summarize("fix\n  the   thing", 40)).To(Equal("fix the thing"))
		Expect(jobs.Summarize("aaaaaaaaaa", 5)).To(Equal("aaaa…"))
	})

	It("reads an unset timestamp as unknown rather than as 1970", func() {
		Expect(jobs.FormatAge(time.Time{})).To(Equal("-"))
		Expect(jobs.FormatAge(time.Now().Add(-90 * time.Minute))).To(Equal("1h30m"))
	})
})
