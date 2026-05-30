// Package reqid provides per-RPC request IDs: a ConnectRPC interceptor that
// generates an ID if the caller did not supply one, attaches it to the request
// context, and echoes it on the response header. Together with LoggerFrom it
// gives every log line emitted while handling an RPC a shared request_id field
// so a single job can be traced across the orchestrator without guessing.
package reqid

import (
	"context"
	"log/slog"
	"net/http"

	"connectrpc.com/connect"
	"github.com/google/uuid"
)

// HeaderName is the canonical HTTP header used to carry the request ID in and
// out of the orchestrator. It matches the de-facto convention used by most
// proxies and observability tooling.
const HeaderName = "X-Request-Id"

type ctxKey struct{}

// WithRequestID returns a context carrying id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the request ID attached to ctx, if any.
func FromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxKey{}).(string)
	return id, ok && id != ""
}

// LoggerFrom returns slog.Default() augmented with whatever observability
// fields are on ctx (currently: request_id). Callers layer their own attrs on
// top with .With(...). Always returns a non-nil logger.
func LoggerFrom(ctx context.Context) *slog.Logger {
	l := slog.Default()
	if id, ok := FromContext(ctx); ok {
		l = l.With("request_id", id)
	}
	return l
}

// generate returns a new request ID. UUIDv7 is time-ordered so log/metric
// scans for a given window remain easy without losing global uniqueness.
func generate() string {
	id, err := uuid.NewV7()
	if err != nil {
		// Extremely unlikely; fall back to v4 so we always have *some* id.
		return uuid.NewString()
	}
	return id.String()
}

// Interceptor generates and propagates request IDs. It is intentionally first
// in the chain so subsequent interceptors (auth, metrics) see the ID on ctx
// and can include it in their own log lines.
type Interceptor struct{}

// NewInterceptor returns a new request-ID interceptor.
func NewInterceptor() *Interceptor { return &Interceptor{} }

// resolve picks the inbound ID off h or mints a new one.
func resolve(h http.Header) string {
	if id := h.Get(HeaderName); id != "" {
		return id
	}
	return generate()
}

// WrapUnary attaches a request ID to ctx and echoes it on the response.
func (*Interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		id := resolve(req.Header())
		ctx = WithRequestID(ctx, id)
		resp, err := next(ctx, req)
		if resp != nil {
			resp.Header().Set(HeaderName, id)
		}
		return resp, err
	}
}

// WrapStreamingClient is a pass-through; this interceptor is server-side only.
func (*Interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler attaches a request ID to ctx and echoes it on the
// response stream's header.
func (*Interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		id := resolve(conn.RequestHeader())
		conn.ResponseHeader().Set(HeaderName, id)
		return next(WithRequestID(ctx, id), conn)
	}
}
