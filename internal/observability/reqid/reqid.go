// Package reqid provides per-RPC request IDs: a ConnectRPC interceptor that
// generates an ID if the caller did not supply one, attaches it to the request
// context, and echoes it on the response header. Together with LoggerFrom it
// gives every log line emitted while handling an RPC a shared request_id field
// so a single job can be traced across the orchestrator without guessing.
package reqid

import (
	"context"
	"log/slog"

	"connectrpc.com/connect/v2"
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

// NewInterceptor returns a server interceptor that generates and propagates
// request IDs. It is intentionally first in the chain so subsequent
// interceptors (auth, metrics) see the ID on ctx and can include it in their
// own log lines.
//
// The ID goes on the response header before the handler runs. connect-go
// writes the response headers on an error reply too, so a caller gets the ID
// back exactly when it most wants something to correlate against.
func NewInterceptor() connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			info, ok := connect.CallInfoForServerContext(ctx)
			if !ok {
				return next(ctx, spec, stream)
			}
			id := resolve(info.RequestHeader())
			info.ResponseHeader().Set(HeaderName, id)
			return next(WithRequestID(ctx, id), spec, stream)
		}
	}
}

// resolve picks the inbound ID off h or mints a new one.
func resolve(h *connect.Header) string {
	if id := h.Get(HeaderName); id != "" {
		return id
	}
	return generate()
}
