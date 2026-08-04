package auth

import (
	"context"
	"errors"
	"log/slog"
	"net"

	"connectrpc.com/connect"
	"github.com/aholstenson/kvarn/internal/config/apikey"
)

// LocalName is the KeyName carried by every host-local identity. It stands
// where a key name would in an audit line.
const LocalName = "local"

// LocalInterceptor authenticates requests arriving on the orchestrator's
// host-local control socket.
//
// The socket is an authentication transport, not an authorization bypass. It
// produces an Identity exactly as the bearer interceptor does, and the handlers
// behind it ask the same questions of both; what differs is only how the claim
// is proven — the filesystem rather than a secret. The socket is created mode
// 0600 inside a 0700 directory, so reaching it already means holding the
// account that owns the orchestrator's config, its sessions database and its
// API key file. Someone with all of that cannot be meaningfully restricted by
// kvarn, which is why the identity carries every capability and an unbounded
// project scope rather than a curated subset that would only be theatre.
//
// Peer credentials are recorded, not enforced. The socket's mode is what keeps
// other accounts out; a uid equality check would add nothing on top of it while
// rejecting root and the sudo-shaped workflows operators actually use. What the
// credentials buy is the audit trail: "who stopped the host" must have an
// answer for a caller that presented no key.
type LocalInterceptor struct {
	metrics AuthMetrics
}

// LocalOption customizes LocalInterceptor construction.
type LocalOption func(*LocalInterceptor)

// WithLocalMetrics wires the auth.attempts counter sink.
func WithLocalMetrics(m AuthMetrics) LocalOption {
	return func(i *LocalInterceptor) { i.metrics = m }
}

// NewLocalInterceptor returns an interceptor for the host-local listener.
func NewLocalInterceptor(opts ...LocalOption) *LocalInterceptor {
	i := &LocalInterceptor{}
	for _, opt := range opts {
		opt(i)
	}
	return i
}

// reasonNotLocal is the audit category for a request that reached the local
// handler without arriving over a unix socket.
const reasonNotLocal = "not_local"

// errNotLocal is returned when the request did not arrive over a unix socket.
// It is deliberately not the opaque errAuthFailed: this cannot be caused by a
// caller getting a credential wrong, only by the local mux being served
// somewhere it must not be, so the message should say so.
var errNotLocal = connect.NewError(connect.CodeUnauthenticated,
	errors.New("host-local control is only available over the local socket"))

// identify builds the identity for a request served on conn. A connection that
// is not a unix socket is refused. That is a structural guard rather than a
// check on the caller: it is what makes serving this mux on a TCP listener fail
// closed instead of publishing host authority to the network. A missing
// connection — ConnContext not installed — fails the same way.
func (i *LocalInterceptor) identify(ctx context.Context) (*Identity, error) {
	conn, ok := ConnFrom(ctx)
	if !ok {
		return nil, errNotLocal
	}
	if _, ok := conn.(*net.UnixConn); !ok {
		return nil, errNotLocal
	}
	return &Identity{
		Source:       SourceLocal,
		KeyName:      LocalName,
		Peer:         peerDescription(conn),
		Projects:     []string{apikey.Wildcard},
		Capabilities: apikey.AllCapabilities(),
	}, nil
}

// record emits the audit event and the metric for one local request.
func (i *LocalInterceptor) record(ctx context.Context, id *Identity, procedure, remote string, err error) {
	if err != nil {
		slog.LogAttrs(ctx, slog.LevelWarn, "local_control_rejected",
			slog.Bool("audit", true),
			slog.String("auth_source", SourceLocal),
			slog.String("reason", reasonNotLocal),
			slog.String("method", procedure),
			slog.String("request_id", reqIDOrEmpty(ctx)),
			slog.String("remote_addr", remote),
		)
		if i.metrics != nil {
			i.metrics.RecordAuth(ctx, "failed", reasonNotLocal)
		}
		return
	}
	slog.LogAttrs(ctx, slog.LevelInfo, "local_control_used", AuditAttrs(ctx, id, procedure, remote)...)
	if i.metrics != nil {
		i.metrics.RecordAuth(ctx, "success", "")
	}
}

// WrapUnary authenticates unary handler calls.
func (i *LocalInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		id, err := i.identify(ctx)
		i.record(ctx, id, req.Spec().Procedure, req.Peer().Addr, err)
		if err != nil {
			return nil, err
		}
		return next(WithIdentity(ctx, id), req)
	}
}

// WrapStreamingClient is a pass-through; this interceptor is server-side only.
func (i *LocalInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler authenticates server-streaming handler calls.
func (i *LocalInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		id, err := i.identify(ctx)
		i.record(ctx, id, conn.Spec().Procedure, conn.Peer().Addr, err)
		if err != nil {
			return err
		}
		return next(WithIdentity(ctx, id), conn)
	}
}

type connKey struct{}

// ConnContext stashes the accepted connection on the base context of every
// request served over it. net/http exposes the connection nowhere else, and the
// local interceptor needs it both to prove the request came in over a unix
// socket and to read the peer's credentials. Assign it to the local listener's
// http.Server.ConnContext.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, connKey{}, c)
}

// ConnFrom returns the connection ConnContext attached to ctx, if any.
func ConnFrom(ctx context.Context) (net.Conn, bool) {
	c, ok := ctx.Value(connKey{}).(net.Conn)
	return c, ok
}
