package orchestrator_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/cmd/client"
	"github.com/aholstenson/kvarn/internal/config/apikey"
	"github.com/aholstenson/kvarn/internal/orchestrator"
	"github.com/aholstenson/kvarn/internal/orchestrator/auth"
	"github.com/aholstenson/kvarn/internal/session"
)

// serveOrchestrator stands up an in-process OrchestratorService over HTTP and
// returns its base address, registering teardown.
func serveOrchestrator(svc *orchestrator.Service, interceptors ...connect.Interceptor) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())

	mux := http.NewServeMux()
	var opts []connect.HandlerOption
	if len(interceptors) > 0 {
		opts = append(opts, connect.WithInterceptors(interceptors...))
	}
	path, handler := kvarnv1connect.NewOrchestratorServiceHandler(svc, opts...)
	mux.Handle(path, handler)
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	DeferCleanup(server.Close)

	return fmt.Sprintf("http://%s", listener.Addr().String())
}

var _ = Describe("OrchestratorService persistent sessions", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	Describe("event history (ListSessionEvents / WatchSession from_sequence)", func() {
		var (
			addr      string
			sessionID string
		)

		BeforeEach(func() {
			mgr := session.NewManager(session.NewMemStore())
			sess, err := mgr.Create(ctx, "proj", "prompt", "auto")
			Expect(err).NotTo(HaveOccurred())
			sessionID = sess.ID

			// Durable history: seqs 1 (cloning), 2 (running), 3 (completed).
			Expect(mgr.UpdateState(ctx, sessionID, session.StateCloning, "1")).To(Succeed())
			Expect(mgr.UpdateState(ctx, sessionID, session.StateRunning, "2")).To(Succeed())
			Expect(mgr.UpdateState(ctx, sessionID, session.StateCompleted, "done")).To(Succeed())

			svc := orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{
				SessionMgr:  mgr,
				AuthEnabled: false,
			})
			addr = serveOrchestrator(svc)
		})

		It("returns full history with sequences and last_sequence via ListSessionEvents", func() {
			oc := client.NewOrchestrator(addr, "")
			resp, err := oc.ListSessionEvents(ctx, connect.NewRequest(&v1.ListSessionEventsRequest{
				SessionId: sessionID,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Events).To(HaveLen(3))
			Expect(resp.Msg.Events[0].Sequence).To(Equal(int64(1)))
			Expect(resp.Msg.Events[2].Sequence).To(Equal(int64(3)))
			Expect(resp.Msg.LastSequence).To(Equal(int64(3)))
			// Mapped through the shared mapper: the final event is a state change
			// to completed.
			sc := resp.Msg.Events[2].GetStateChange()
			Expect(sc).NotTo(BeNil())
			Expect(sc.State).To(Equal("completed"))
		})

		It("honors after_sequence when polling", func() {
			oc := client.NewOrchestrator(addr, "")
			resp, err := oc.ListSessionEvents(ctx, connect.NewRequest(&v1.ListSessionEventsRequest{
				SessionId:     sessionID,
				AfterSequence: 2,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Events).To(HaveLen(1))
			Expect(resp.Msg.Events[0].Sequence).To(Equal(int64(3)))
			Expect(resp.Msg.LastSequence).To(Equal(int64(3)))
		})

		It("resumes from from_sequence on WatchSession", func() {
			oc := client.NewOrchestrator(addr, "")
			stream, err := oc.WatchSession(ctx, connect.NewRequest(&v1.WatchSessionRequest{
				SessionId:    sessionID,
				FromSequence: 2,
			}))
			Expect(err).NotTo(HaveOccurred())
			defer stream.Close()

			var seqs []int64
			for stream.Receive() {
				seqs = append(seqs, stream.Msg().Sequence)
			}
			Expect(stream.Err()).NotTo(HaveOccurred())
			Expect(seqs).To(Equal([]int64{3}))
		})
	})

	Describe("ListSessions pagination with identity under-fill", func() {
		It("fills each page with authorized rows despite interleaved denied projects", func() {
			// Craft sessions with controlled timestamps so ordering is
			// deterministic (created_at DESC). Interleave an allowed project "a"
			// with a denied project "b".
			store := session.NewMemStore()
			base := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
			order := []struct {
				id      string
				project string
			}{
				{"a1", "a"}, {"b1", "b"}, {"a2", "a"}, {"b2", "b"}, {"a3", "a"},
			}
			for i, o := range order {
				Expect(store.CreateSession(ctx, &session.Session{
					ID:          o.id,
					ProjectName: o.project,
					Prompt:      "p",
					Mode:        "auto",
					State:       session.StateRunning,
					CreatedAt:   base.Add(time.Duration(i) * time.Minute),
					UpdatedAt:   base.Add(time.Duration(i) * time.Minute),
				})).To(Succeed())
			}
			mgr := session.NewManager(store)

			apiKeyStore := &memAPIKeyStore{keys: map[string]*apikey.APIKey{}}
			scopedToken := addKey(apiKeyStore, "scoped", "a")

			svc := orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{
				SessionMgr:  mgr,
				APIKeyStore: apiKeyStore,
				AuthEnabled: true,
			})
			addr := serveOrchestrator(svc, auth.NewInterceptor(apiKeyStore))

			oc := client.NewOrchestrator(addr, scopedToken)

			// Page 1: newest two allowed rows (a3, a2), not under-filled to one.
			page1, err := oc.ListSessions(ctx, connect.NewRequest(&v1.ListSessionsRequest{Limit: 2}))
			Expect(err).NotTo(HaveOccurred())
			ids1 := projectIDs(page1.Msg.Sessions)
			Expect(ids1).To(Equal([]string{"a3", "a2"}))
			Expect(page1.Msg.NextPageCursor).NotTo(BeEmpty())

			// Page 2: the remaining allowed row (a1).
			page2, err := oc.ListSessions(ctx, connect.NewRequest(&v1.ListSessionsRequest{
				Limit:      2,
				PageCursor: page1.Msg.NextPageCursor,
			}))
			Expect(err).NotTo(HaveOccurred())
			ids2 := projectIDs(page2.Msg.Sessions)
			Expect(ids2).To(Equal([]string{"a1"}))
		})
	})
})

func projectIDs(sessions []*v1.GetSessionResponse) []string {
	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		ids = append(ids, s.SessionId)
	}
	return ids
}
