package orchestrator_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/cmd/client"
	"github.com/aholstenson/kvarn/internal/config/apikey"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/orchestrator"
	"github.com/aholstenson/kvarn/internal/orchestrator/auth"
	"github.com/aholstenson/kvarn/internal/session"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// memAPIKeyStore is an in-memory apikey.Store for authorization tests.
type memAPIKeyStore struct {
	keys map[string]*apikey.APIKey
}

func (m *memAPIKeyStore) Get(_ context.Context, keyID string) (*apikey.APIKey, error) {
	k, ok := m.keys[keyID]
	if !ok {
		return nil, apikey.ErrNotFound
	}
	return k, nil
}

func (m *memAPIKeyStore) List(_ context.Context) ([]*apikey.APIKey, error) {
	var out []*apikey.APIKey
	for _, k := range m.keys {
		out = append(out, k)
	}
	return out, nil
}

func (m *memAPIKeyStore) Put(_ context.Context, k *apikey.APIKey) error {
	m.keys[k.KeyID] = k
	return nil
}

func (m *memAPIKeyStore) Delete(_ context.Context, keyID string) error {
	delete(m.keys, keyID)
	return nil
}

// addKey mints a key scoped to the given projects and returns its full token.
func addKey(store *memAPIKeyStore, name string, projects ...string) string {
	token, keyID, hash, err := apikey.GenerateToken()
	Expect(err).NotTo(HaveOccurred())
	store.keys[keyID] = &apikey.APIKey{
		KeyID:    keyID,
		Name:     name,
		Hash:     hash,
		Projects: projects,
		Created:  time.Now().UTC(),
	}
	return token
}

var _ = Describe("OrchestratorService authorization", func() {
	var (
		server         *http.Server
		listener       net.Listener
		addr           string
		sessionMgr     *session.MemoryManager
		allowedSession string
		otherSession   string
		wildcardToken  string
		scopedToken    string
	)

	BeforeEach(func() {
		ctx := context.Background()

		apiKeyStore := &memAPIKeyStore{keys: map[string]*apikey.APIKey{}}
		wildcardToken = addKey(apiKeyStore, "wild", "*")
		scopedToken = addKey(apiKeyStore, "scoped", "allowed-project")

		sessionMgr = session.NewMemoryManager()
		s1, err := sessionMgr.Create(ctx, "allowed-project", "prompt", "auto")
		Expect(err).NotTo(HaveOccurred())
		allowedSession = s1.ID
		s2, err := sessionMgr.Create(ctx, "other-project", "prompt", "auto")
		Expect(err).NotTo(HaveOccurred())
		otherSession = s2.ID

		projStore := &memProjectStore{projects: map[string]*project.Project{
			"allowed-project": {Name: "allowed-project", RepoURL: "/nonexistent", DefaultBranch: "main"},
		}}

		svc := orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{
			ProjectStore: projStore,
			SessionMgr:   sessionMgr,
			APIKeyStore:  apiKeyStore,
			AuthEnabled:  true,
		})

		listener, err = net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		addr = fmt.Sprintf("http://%s", listener.Addr().String())

		mux := http.NewServeMux()
		path, handler := kvarnv1connect.NewOrchestratorServiceHandler(svc,
			connect.WithInterceptors(auth.NewInterceptor(apiKeyStore)))
		mux.Handle(path, handler)

		server = &http.Server{Handler: mux}
		go server.Serve(listener)
	})

	AfterEach(func() {
		server.Close()
	})

	Describe("StartJob", func() {
		It("rejects requests without a token", func() {
			oc := client.NewOrchestrator(addr, "")
			_, err := oc.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project: "allowed-project", Prompt: "do",
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
		})

		It("denies a project outside the key's scope", func() {
			oc := client.NewOrchestrator(addr, scopedToken)
			_, err := oc.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project: "other-project", Prompt: "do",
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		})

		It("allows an in-scope project", func() {
			oc := client.NewOrchestrator(addr, scopedToken)
			resp, err := oc.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
				Project: "allowed-project", Prompt: "do",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.SessionId).NotTo(BeEmpty())
		})
	})

	Describe("GetSession", func() {
		It("allows an in-scope session", func() {
			oc := client.NewOrchestrator(addr, scopedToken)
			resp, err := oc.GetSession(context.Background(), connect.NewRequest(&v1.GetSessionRequest{
				SessionId: allowedSession,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Project).To(Equal("allowed-project"))
		})

		It("denies a session outside the key's scope", func() {
			oc := client.NewOrchestrator(addr, scopedToken)
			_, err := oc.GetSession(context.Background(), connect.NewRequest(&v1.GetSessionRequest{
				SessionId: otherSession,
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		})
	})

	Describe("WatchSession", func() {
		It("denies a session outside the key's scope", func() {
			oc := client.NewOrchestrator(addr, scopedToken)
			stream, err := oc.WatchSession(context.Background(), connect.NewRequest(&v1.WatchSessionRequest{
				SessionId: otherSession,
			}))
			Expect(err).NotTo(HaveOccurred())
			defer stream.Close()
			Expect(stream.Receive()).To(BeFalse())
			Expect(connect.CodeOf(stream.Err())).To(Equal(connect.CodePermissionDenied))
		})
	})

	Describe("ListSessions", func() {
		It("returns only the sessions the scoped key may access", func() {
			oc := client.NewOrchestrator(addr, scopedToken)
			resp, err := oc.ListSessions(context.Background(), connect.NewRequest(&v1.ListSessionsRequest{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Sessions).To(HaveLen(1))
			Expect(resp.Msg.Sessions[0].Project).To(Equal("allowed-project"))
		})

		It("returns all sessions for a wildcard key", func() {
			oc := client.NewOrchestrator(addr, wildcardToken)
			resp, err := oc.ListSessions(context.Background(), connect.NewRequest(&v1.ListSessionsRequest{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Sessions).To(HaveLen(2))
		})
	})
})

var _ = Describe("OrchestratorService with auth disabled", func() {
	var (
		server   *http.Server
		listener net.Listener
		addr     string
	)

	BeforeEach(func() {
		ctx := context.Background()
		sessionMgr := session.NewMemoryManager()
		_, err := sessionMgr.Create(ctx, "p1", "prompt", "auto")
		Expect(err).NotTo(HaveOccurred())
		_, err = sessionMgr.Create(ctx, "p2", "prompt", "auto")
		Expect(err).NotTo(HaveOccurred())

		svc := orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{
			SessionMgr:  sessionMgr,
			AuthEnabled: false,
		})

		listener, err = net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		addr = fmt.Sprintf("http://%s", listener.Addr().String())

		mux := http.NewServeMux()
		path, handler := kvarnv1connect.NewOrchestratorServiceHandler(svc)
		mux.Handle(path, handler)
		server = &http.Server{Handler: mux}
		go server.Serve(listener)
	})

	AfterEach(func() {
		server.Close()
	})

	It("returns every session without a token", func() {
		oc := client.NewOrchestrator(addr, "")
		resp, err := oc.ListSessions(context.Background(), connect.NewRequest(&v1.ListSessionsRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.Sessions).To(HaveLen(2))
	})
})
