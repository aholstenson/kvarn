package reqid_test

import (
	"context"
	"net/http"
	"net/http/httptest"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/observability/reqid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const procedure = "/svc/Method"

// call dispatches one RPC through interceptor to next the way connect.Server
// does, so the interceptor sees info on its context.
func call(interceptor connect.ServerInterceptor, info *connect.CallInfo, next connect.ServerFunc) error {
	server := connect.NewServer(interceptor)
	server.Register(connect.Method{Spec: connect.Spec{Procedure: procedure}, Handler: next})
	return server.Call(context.Background(), procedure, info, nil)
}

var _ = Describe("Interceptor", func() {
	var (
		interceptor connect.ServerInterceptor
		info        *connect.CallInfo
	)

	BeforeEach(func() {
		interceptor = reqid.NewInterceptor()
		info = &connect.CallInfo{}
	})

	It("generates a request ID when none is supplied and echoes it on the response", func() {
		var seenID string
		err := call(interceptor, info, func(ctx context.Context, _ connect.Spec, _ connect.ServerStream) error {
			id, ok := reqid.FromContext(ctx)
			Expect(ok).To(BeTrue())
			seenID = id
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(seenID).NotTo(BeEmpty())
		Expect(info.ResponseHeader().Get(reqid.HeaderName)).To(Equal(seenID))
	})

	It("preserves an inbound X-Request-Id", func() {
		info.RequestHeader().Set(reqid.HeaderName, "from-caller")
		var seenID string
		err := call(interceptor, info, func(ctx context.Context, _ connect.Spec, _ connect.ServerStream) error {
			seenID, _ = reqid.FromContext(ctx)
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(seenID).To(Equal("from-caller"))
		Expect(info.ResponseHeader().Get(reqid.HeaderName)).To(Equal("from-caller"))
	})

	It("passes the handler's error through unchanged", func() {
		want := connect.NewError(connect.CodePermissionDenied, "denied")
		err := call(interceptor, info, func(context.Context, connect.Spec, connect.ServerStream) error {
			return want
		})
		Expect(err).To(MatchError(want))
	})

	// A failed call is exactly when a caller most wants something to correlate
	// against, so the ID has to reach the wire on an error reply too.
	It("echoes the request ID to the caller when the handler fails", func() {
		server := connect.NewServer(interceptor)
		kvarnv1connect.RegisterOrchestratorServiceHandler(server, kvarnv1connect.UnimplementedOrchestratorServiceHandler{})
		mux := http.NewServeMux()
		connecthttp.Mount(mux, server)
		httpServer := httptest.NewServer(mux)
		DeferCleanup(httpServer.Close)

		client := kvarnv1connect.NewOrchestratorServiceClient(connect.NewClient(
			connecthttp.NewTransport(httpServer.Client(), httpServer.URL),
		))
		ctx, callInfo := connect.NewClientContext(context.Background())
		callInfo.RequestHeader().Set(reqid.HeaderName, "from-caller")

		_, err := client.GetSession(ctx, &v1.GetSessionRequest{})
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnimplemented))
		Expect(callInfo.ResponseHeader().Get(reqid.HeaderName)).To(Equal("from-caller"))
	})

	It("LoggerFrom returns a usable logger even without a request_id", func() {
		Expect(reqid.LoggerFrom(context.Background())).NotTo(BeNil())
	})
})
