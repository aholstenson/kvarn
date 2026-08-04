package reqid_test

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	"github.com/aholstenson/kvarn/internal/observability/reqid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// fakeRequest is the minimal connect.AnyRequest the interceptor reads from.
type fakeRequest struct {
	connect.Request[struct{}]
	hdr  http.Header
	spec connect.Spec
	peer connect.Peer
}

func (r *fakeRequest) Any() any            { return nil }
func (r *fakeRequest) Spec() connect.Spec  { return r.spec }
func (r *fakeRequest) Peer() connect.Peer  { return r.peer }
func (r *fakeRequest) Header() http.Header { return r.hdr }
func (r *fakeRequest) HTTPMethod() string  { return http.MethodPost }

// fakeResponse captures headers set by the interceptor.
type fakeResponse struct {
	connect.Response[struct{}]
	hdr http.Header
}

func (r *fakeResponse) Any() any            { return nil }
func (r *fakeResponse) Header() http.Header { return r.hdr }
func (r *fakeResponse) Trailer() http.Header {
	if r.Response.Trailer() == nil {
		return http.Header{}
	}
	return r.Response.Trailer()
}

var _ = Describe("Interceptor", func() {
	var (
		interceptor connect.Interceptor
		req         *fakeRequest
	)

	BeforeEach(func() {
		interceptor = reqid.NewInterceptor()
		req = &fakeRequest{
			hdr:  http.Header{},
			spec: connect.Spec{Procedure: "/svc/Method", IsClient: false},
		}
	})

	It("generates a request ID when none is supplied and echoes it on the response", func() {
		var seenID string
		resp := &fakeResponse{hdr: http.Header{}}
		next := connect.UnaryFunc(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
			id, ok := reqid.FromContext(ctx)
			Expect(ok).To(BeTrue())
			seenID = id
			return resp, nil
		})

		_, err := interceptor.WrapUnary(next)(context.Background(), req)
		Expect(err).NotTo(HaveOccurred())
		Expect(seenID).NotTo(BeEmpty())
		Expect(resp.Header().Get(reqid.HeaderName)).To(Equal(seenID))
	})

	It("preserves an inbound X-Request-Id", func() {
		req.hdr.Set(reqid.HeaderName, "from-caller")
		resp := &fakeResponse{hdr: http.Header{}}
		var seenID string
		next := connect.UnaryFunc(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
			seenID, _ = reqid.FromContext(ctx)
			return resp, nil
		})

		_, err := interceptor.WrapUnary(next)(context.Background(), req)
		Expect(err).NotTo(HaveOccurred())
		Expect(seenID).To(Equal("from-caller"))
		Expect(resp.Header().Get(reqid.HeaderName)).To(Equal("from-caller"))
	})

	// A failing handler hands back a typed-nil *connect.Response, which boxes
	// into a non-nil AnyResponse. Reading a header off it panics, which took
	// down the connection instead of returning the error the handler chose.
	It("survives a handler that returns an error with a typed-nil response", func() {
		want := connect.NewError(connect.CodePermissionDenied, errors.New("denied"))
		next := connect.UnaryFunc(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
			var nilResp *connect.Response[struct{}]
			return nilResp, want
		})

		resp, err := interceptor.WrapUnary(next)(context.Background(), req)
		Expect(resp).To(BeNil())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
	})

	It("echoes the request ID on an error's metadata", func() {
		req.hdr.Set(reqid.HeaderName, "from-caller")
		cerr := connect.NewError(connect.CodeNotFound, errors.New("nope"))
		next := connect.UnaryFunc(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
			return nil, cerr
		})

		_, err := interceptor.WrapUnary(next)(context.Background(), req)
		Expect(err).To(HaveOccurred())
		var got *connect.Error
		Expect(errors.As(err, &got)).To(BeTrue())
		Expect(got.Meta().Get(reqid.HeaderName)).To(Equal("from-caller"))
	})

	It("LoggerFrom returns a usable logger even without a request_id", func() {
		Expect(reqid.LoggerFrom(context.Background())).NotTo(BeNil())
	})
})
