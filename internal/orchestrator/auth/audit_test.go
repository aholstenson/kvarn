package auth_test

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/aholstenson/kvarn/internal/config/apikey"
	"github.com/aholstenson/kvarn/internal/orchestrator/auth"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// captureHandler records every record passed to slog.Default for assertion.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (c *captureHandler) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r.Clone())
	return nil
}
func (c *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *captureHandler) WithGroup(string) slog.Handler      { return c }
func (c *captureHandler) match(msg string) (slog.Record, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if r.Message == msg {
			return r, true
		}
	}
	return slog.Record{}, false
}

// memStore is a tiny in-memory apikey.Store for these tests.
type memStore struct {
	keys map[string]*apikey.APIKey
}

func (m *memStore) Get(_ context.Context, keyID string) (*apikey.APIKey, error) {
	k, ok := m.keys[keyID]
	if !ok {
		return nil, apikey.ErrNotFound
	}
	return k, nil
}
func (m *memStore) List(context.Context) ([]*apikey.APIKey, error) { return nil, nil }
func (m *memStore) Put(context.Context, *apikey.APIKey) error      { return nil }
func (m *memStore) Delete(context.Context, string) error           { return nil }

// fakeReq is a tiny stand-in for the bits of connect.AnyRequest the
// interceptor reads from.
type fakeReq struct {
	connect.Request[struct{}]
	hdr  http.Header
	spec connect.Spec
	peer connect.Peer
}

func (r *fakeReq) Any() any            { return nil }
func (r *fakeReq) Spec() connect.Spec  { return r.spec }
func (r *fakeReq) Peer() connect.Peer  { return r.peer }
func (r *fakeReq) Header() http.Header { return r.hdr }
func (r *fakeReq) HTTPMethod() string  { return http.MethodPost }

func attrMap(r slog.Record) map[string]any {
	out := map[string]any{}
	r.Attrs(func(a slog.Attr) bool {
		out[a.Key] = a.Value.Any()
		return true
	})
	return out
}

var _ = Describe("Auth audit logging", func() {
	var (
		previous *slog.Logger
		capture  *captureHandler
		token    string
		keyID    string
		hash     string
		store    *memStore
	)

	BeforeEach(func() {
		var err error
		token, keyID, hash, err = apikey.GenerateToken()
		Expect(err).NotTo(HaveOccurred())
		store = &memStore{keys: map[string]*apikey.APIKey{
			keyID: {KeyID: keyID, Name: "ci", Hash: hash, Projects: []string{"*"}, Created: time.Now().UTC()},
		}}
		capture = &captureHandler{}
		previous = slog.Default()
		slog.SetDefault(slog.New(capture))
	})

	AfterEach(func() {
		slog.SetDefault(previous)
	})

	It("emits api_key_used on a successful unary auth", func() {
		interceptor := auth.NewInterceptor(store)
		req := &fakeReq{
			hdr:  http.Header{"Authorization": []string{"Bearer " + token}},
			spec: connect.Spec{Procedure: "/svc/Method"},
			peer: connect.Peer{Addr: "10.0.0.1"},
		}
		next := connect.UnaryFunc(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
			return connect.NewResponse(&struct{}{}), nil
		})
		_, err := interceptor.WrapUnary(next)(context.Background(), req)
		Expect(err).NotTo(HaveOccurred())

		rec, ok := capture.match("api_key_used")
		Expect(ok).To(BeTrue())
		attrs := attrMap(rec)
		Expect(attrs["audit"]).To(Equal(true))
		Expect(attrs["key_name"]).To(Equal("ci"))
		Expect(attrs["key_id"]).To(Equal(keyID))
		Expect(attrs["method"]).To(Equal("/svc/Method"))
		Expect(attrs["remote_addr"]).To(Equal("10.0.0.1"))
	})

	It("emits api_key_auth_failed with a reason on rejection", func() {
		interceptor := auth.NewInterceptor(store)
		req := &fakeReq{
			hdr:  http.Header{"Authorization": []string{"Bearer not-a-real-token"}},
			spec: connect.Spec{Procedure: "/svc/Method"},
			peer: connect.Peer{Addr: "10.0.0.2"},
		}
		_, err := interceptor.WrapUnary(connect.UnaryFunc(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
			return nil, nil
		}))(context.Background(), req)
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))

		rec, ok := capture.match("api_key_auth_failed")
		Expect(ok).To(BeTrue())
		attrs := attrMap(rec)
		Expect(attrs["reason"]).To(Equal("invalid_format"))
		Expect(attrs["method"]).To(Equal("/svc/Method"))
		Expect(attrs["audit"]).To(Equal(true))
	})
})
