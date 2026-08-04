package auth_test

import (
	"context"
	"log/slog"
	"net"
	"path/filepath"

	"connectrpc.com/connect"
	"github.com/aholstenson/kvarn/internal/config/apikey"
	"github.com/aholstenson/kvarn/internal/orchestrator/auth"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// unixPair returns a connected pair of unix-socket endpoints, standing in for
// the connection the local listener would have accepted.
func unixPair() (server, client net.Conn) {
	path := filepath.Join(GinkgoT().TempDir(), "s")
	l, err := net.Listen("unix", path)
	Expect(err).NotTo(HaveOccurred())
	defer l.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, aerr := l.Accept()
		if aerr == nil {
			accepted <- c
		}
		close(accepted)
	}()

	client, err = net.Dial("unix", path)
	Expect(err).NotTo(HaveOccurred())
	server = <-accepted
	Expect(server).NotTo(BeNil())
	DeferCleanup(func() {
		server.Close()
		client.Close()
	})
	return server, client
}

var _ = Describe("LocalInterceptor", func() {
	var (
		interceptor connect.Interceptor
		req         *fakeReq
	)

	BeforeEach(func() {
		interceptor = auth.NewLocalInterceptor()
		req = &fakeReq{spec: connect.Spec{Procedure: "/svc/Method"}}
	})

	// Arriving on the socket is the whole claim, and what it buys is host
	// authority — a caller who owns the orchestrator's config directory cannot
	// be meaningfully restricted by kvarn.
	It("grants host capability and an unbounded scope over a unix socket", func() {
		conn, _ := unixPair()
		ctx := auth.ConnContext(context.Background(), conn)

		var got *auth.Identity
		next := connect.UnaryFunc(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
			id, ok := auth.IdentityFrom(ctx)
			Expect(ok).To(BeTrue())
			got = id
			return connect.NewResponse(&struct{}{}), nil
		})

		_, err := interceptor.WrapUnary(next)(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Source).To(Equal(auth.SourceLocal))
		Expect(got.Allows(apikey.CapabilityHost)).To(BeTrue())
		Expect(got.IsWildcard()).To(BeTrue())
		Expect(got.KeyID).To(BeEmpty())
	})

	// The structural guard: serving this mux on a TCP listener must fail
	// closed rather than publish host authority to the network.
	It("refuses a request that did not arrive over a unix socket", func() {
		server, client := net.Pipe()
		DeferCleanup(func() {
			server.Close()
			client.Close()
		})
		ctx := auth.ConnContext(context.Background(), server)

		called := false
		next := connect.UnaryFunc(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
			called = true
			return connect.NewResponse(&struct{}{}), nil
		})

		_, err := interceptor.WrapUnary(next)(ctx, req)
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
		Expect(called).To(BeFalse())
	})

	// Without ConnContext installed there is no connection to inspect, so the
	// interceptor cannot tell where the request came from and must not guess.
	It("refuses a request with no connection on the context", func() {
		called := false
		next := connect.UnaryFunc(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
			called = true
			return connect.NewResponse(&struct{}{}), nil
		})

		_, err := interceptor.WrapUnary(next)(context.Background(), req)
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
		Expect(called).To(BeFalse())
	})

	Describe("audit logging", func() {
		var (
			previous *slog.Logger
			capture  *captureHandler
		)

		BeforeEach(func() {
			capture = &captureHandler{}
			previous = slog.Default()
			slog.SetDefault(slog.New(capture))
			DeferCleanup(func() { slog.SetDefault(previous) })
		})

		// "Who stopped the host" has to have an answer for a caller that
		// presented no key, which is what the peer credentials are for.
		It("records the local caller and its peer", func() {
			conn, _ := unixPair()
			ctx := auth.ConnContext(context.Background(), conn)
			next := connect.UnaryFunc(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
				return connect.NewResponse(&struct{}{}), nil
			})
			_, err := interceptor.WrapUnary(next)(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			rec, ok := capture.match("local_control_used")
			Expect(ok).To(BeTrue())
			attrs := attrMap(rec)
			Expect(attrs["audit"]).To(Equal(true))
			Expect(attrs["auth_source"]).To(Equal(auth.SourceLocal))
			Expect(attrs["key_name"]).To(Equal(auth.LocalName))
			Expect(attrs["method"]).To(Equal("/svc/Method"))
			Expect(attrs["peer"]).To(ContainSubstring("uid="))
		})

		It("records a rejection", func() {
			_, err := interceptor.WrapUnary(connect.UnaryFunc(
				func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
					return connect.NewResponse(&struct{}{}), nil
				}))(context.Background(), req)
			Expect(err).To(HaveOccurred())

			rec, ok := capture.match("local_control_rejected")
			Expect(ok).To(BeTrue())
			Expect(attrMap(rec)["audit"]).To(Equal(true))
		})
	})
})
