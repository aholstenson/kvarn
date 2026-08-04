package orchestrator_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/cmd/client"
	"github.com/aholstenson/kvarn/internal/config/apikey"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/localsock"
	"github.com/aholstenson/kvarn/internal/orchestrator"
	"github.com/aholstenson/kvarn/internal/orchestrator/auth"
	"github.com/aholstenson/kvarn/internal/session"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// addKeyWithCapabilities mints a key with both axes set and returns its token.
func addKeyWithCapabilities(store *memAPIKeyStore, name string, projects []string, caps ...apikey.Capability) string {
	token, keyID, hash, err := apikey.GenerateToken()
	Expect(err).NotTo(HaveOccurred())
	store.keys[keyID] = &apikey.APIKey{
		KeyID:        keyID,
		Name:         name,
		Hash:         hash,
		Projects:     projects,
		Capabilities: caps,
		Created:      time.Now().UTC(),
	}
	return token
}

var _ = Describe("Host capability", func() {
	var (
		tcpServer  *http.Server
		tcpAddr    string
		sockServer *http.Server
		sockAddr   string

		wildcardToken     string
		wildcardHostToken string
		scopedToken       string
	)

	BeforeEach(func() {
		ctx := context.Background()

		apiKeyStore := &memAPIKeyStore{keys: map[string]*apikey.APIKey{}}
		wildcardToken = addKeyWithCapabilities(apiKeyStore, "wild", []string{"*"})
		wildcardHostToken = addKeyWithCapabilities(apiKeyStore, "ops", []string{"*"}, apikey.CapabilityHost)
		scopedToken = addKeyWithCapabilities(apiKeyStore, "scoped", []string{"allowed-project"})

		sessionMgr := session.NewManager(session.NewMemStore())
		for _, name := range []string{"allowed-project", "other-project"} {
			_, err := sessionMgr.Create(ctx, session.CreateParams{
				ProjectName: name, Prompt: "prompt", Mode: "auto",
			})
			Expect(err).NotTo(HaveOccurred())
		}

		svc := orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{
			ProjectStore: &memProjectStore{projects: map[string]*project.Project{
				"allowed-project": {Name: "allowed-project", RepoURL: "/nonexistent", DefaultBranch: "main"},
			}},
			SessionMgr:  sessionMgr,
			APIKeyStore: apiKeyStore,
			AuthEnabled: true,
		})

		tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		tcpAddr = fmt.Sprintf("http://%s", tcpListener.Addr().String())
		tcpServer = &http.Server{Handler: h2c.NewHandler(orchestrator.PublicMux(svc), &http2.Server{})}
		go tcpServer.Serve(tcpListener)

		sockPath := filepath.Join(GinkgoT().TempDir(), "orchestrator.sock")
		sockListener, err := localsock.Listen(sockPath)
		Expect(err).NotTo(HaveOccurred())
		sockAddr = localsock.Address(sockPath)
		sockServer = &http.Server{
			Handler:     h2c.NewHandler(orchestrator.LocalMux(svc), &http2.Server{}),
			ConnContext: auth.ConnContext,
		}
		go sockServer.Serve(sockListener)

		DeferCleanup(func() {
			tcpServer.Close()
			sockServer.Close()
		})
	})

	cancelAll := func(addr, token string) error {
		oc := client.NewOrchestrator(addr, token)
		_, err := oc.CancelJobs(context.Background(), connect.NewRequest(&v1.CancelJobsRequest{
			All: true, DryRun: true,
		}))
		return err
	}

	Describe("an unfiltered bulk cancel", func() {
		// The wildcard exists so one key can drive every project, which is what
		// a CI bot needs. Stopping every job on the host is a different claim.
		It("is denied to a wildcard key without the capability", func() {
			err := cancelAll(tcpAddr, wildcardToken)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
			Expect(err.Error()).To(ContainSubstring("host"))
		})

		It("is allowed to a wildcard key holding the capability", func() {
			Expect(cancelAll(tcpAddr, wildcardHostToken)).To(Succeed())
		})

		// A key scoped to named projects reaches only its own work whatever
		// filter it passes, so it claims nothing new and needs nothing new.
		It("is allowed to a project-scoped key", func() {
			Expect(cancelAll(tcpAddr, scopedToken)).To(Succeed())
		})

		It("is allowed over the host-local socket with no key at all", func() {
			Expect(cancelAll(sockAddr, "")).To(Succeed())
		})
	})

	// Draining has no project to scope it to at all — it is the orchestrator's
	// own admission stance — so it is the capability's clearest case.
	Describe("SetDrain", func() {
		setDrain := func(addr, token string) error {
			oc := client.NewOrchestrator(addr, token)
			_, err := oc.SetDrain(context.Background(), connect.NewRequest(&v1.SetDrainRequest{
				Draining: true, Reason: "test",
			}))
			return err
		}

		It("is denied to a wildcard key without the capability", func() {
			Expect(connect.CodeOf(setDrain(tcpAddr, wildcardToken))).To(Equal(connect.CodePermissionDenied))
		})

		It("is denied to a project-scoped key, however many projects it covers", func() {
			Expect(connect.CodeOf(setDrain(tcpAddr, scopedToken))).To(Equal(connect.CodePermissionDenied))
		})

		It("is allowed to a key holding the capability", func() {
			Expect(setDrain(tcpAddr, wildcardHostToken)).To(Succeed())
		})

		It("is allowed over the host-local socket with no key at all", func() {
			Expect(setDrain(sockAddr, "")).To(Succeed())
		})
	})

	// Naming a project puts the request back inside the project axis, where
	// the key's own scope is the whole answer.
	It("allows a project-filtered bulk cancel without the capability", func() {
		oc := client.NewOrchestrator(tcpAddr, wildcardToken)
		_, err := oc.CancelJobs(context.Background(), connect.NewRequest(&v1.CancelJobsRequest{
			Project: "allowed-project", DryRun: true,
		}))
		Expect(err).NotTo(HaveOccurred())
	})

	// Ordinary project-scoped work is unchanged by the new axis: a key with no
	// capabilities still submits and reads jobs exactly as before.
	It("leaves project-scoped RPCs alone", func() {
		oc := client.NewOrchestrator(tcpAddr, scopedToken)
		resp, err := oc.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
			Project: "allowed-project", Prompt: "do",
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.SessionId).NotTo(BeEmpty())
	})

	// The socket is the authentication mechanism, so the mux behind it must
	// fail closed anywhere else rather than publish host authority.
	It("refuses local-mux requests that arrive over TCP", func() {
		svc := orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{
			SessionMgr:  session.NewManager(session.NewMemStore()),
			AuthEnabled: true,
		})
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		srv := &http.Server{
			Handler:     h2c.NewHandler(orchestrator.LocalMux(svc), &http2.Server{}),
			ConnContext: auth.ConnContext,
		}
		go srv.Serve(listener)
		DeferCleanup(func() { srv.Close() })

		oc := client.NewOrchestrator(fmt.Sprintf("http://%s", listener.Addr().String()), "")
		_, err = oc.GetQueueStats(context.Background(), connect.NewRequest(&v1.GetQueueStatsRequest{}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
	})
})
