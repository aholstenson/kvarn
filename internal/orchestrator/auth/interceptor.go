package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/aholstenson/kvarn/internal/config/apikey"
	"github.com/aholstenson/kvarn/internal/observability/reqid"
)

const bearerPrefix = "Bearer "

// AuthMetrics is the minimal sink the interceptor needs for the auth.attempts
// counter. Kept as an interface so package observability/metrics does not have
// to be imported here (avoiding a dep cycle in tests). nil sinks are skipped.
type AuthMetrics interface {
	RecordAuth(ctx context.Context, outcome, reason string)
}

// Interceptor authenticates external OrchestratorService requests against the
// API key store and injects the resulting Identity into the request context.
// It is only installed on the public handler; the host-local BridgeService is
// left unauthenticated.
type Interceptor struct {
	store   apikey.Store
	metrics AuthMetrics
}

// Option customizes Interceptor construction.
type Option func(*Interceptor)

// WithMetrics wires the auth.attempts counter sink.
func WithMetrics(m AuthMetrics) Option { return func(i *Interceptor) { i.metrics = m } }

// NewInterceptor returns an Interceptor backed by store.
func NewInterceptor(store apikey.Store, opts ...Option) *Interceptor {
	i := &Interceptor{store: store}
	for _, opt := range opts {
		opt(i)
	}
	return i
}

// Rejection reason categories. These are coarse enough not to leak which
// specific check failed to the caller (the wire error stays opaque) but
// fine-grained enough for operators reading audit logs / metrics.
const (
	reasonHeaderMissing = "header_missing"
	reasonInvalidFormat = "invalid_format"
	reasonUnknownKey    = "unknown_key"
	reasonHashMismatch  = "hash_mismatch"
	reasonDisabled      = "disabled"
	reasonExpired       = "expired"
	reasonStoreError    = "store_error"
)

// authenticate validates the Authorization header and returns the caller's
// Identity plus the parsed key ID (for audit logging). Every recognizable
// rejection returns errAuthFailed plus a category string so callers can record
// the reason without leaking it to the wire; a store failure maps to
// Unavailable so the service fails closed.
func (i *Interceptor) authenticate(header http.Header) (*Identity, string, string, error) {
	authz := header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		return nil, "", reasonHeaderMissing, errAuthFailed
	}

	keyID, secret, ok := apikey.ParseToken(strings.TrimPrefix(authz, bearerPrefix))
	if !ok {
		return nil, "", reasonInvalidFormat, errAuthFailed
	}

	key, err := i.store.Get(context.Background(), keyID)
	if err != nil {
		if errors.Is(err, apikey.ErrNotFound) {
			return nil, keyID, reasonUnknownKey, errAuthFailed
		}
		return nil, keyID, reasonStoreError,
			connect.NewError(connect.CodeUnavailable, fmt.Errorf("read api key store: %w", err))
	}

	if subtle.ConstantTimeCompare([]byte(apikey.HashSecret(secret)), []byte(key.Hash)) != 1 {
		return nil, keyID, reasonHashMismatch, errAuthFailed
	}
	if key.Disabled {
		return nil, keyID, reasonDisabled, errAuthFailed
	}
	if key.Expired(time.Now()) {
		return nil, keyID, reasonExpired, errAuthFailed
	}

	return &Identity{KeyID: keyID, KeyName: key.Name, Projects: key.Projects}, keyID, "", nil
}

// errAuthFailed is the single opaque error returned for every authentication
// failure to avoid leaking which check rejected the request.
var errAuthFailed = connect.NewError(connect.CodeUnauthenticated, errors.New("authentication failed"))

// logSuccess emits the audit event after a successful authentication.
func (i *Interceptor) logSuccess(ctx context.Context, id *Identity, keyID, procedure, remote string) {
	slog.LogAttrs(ctx, slog.LevelInfo, "api_key_used",
		slog.Bool("audit", true),
		slog.String("key_name", id.KeyName),
		slog.String("key_id", keyID),
		slog.String("method", procedure),
		slog.String("request_id", reqIDOrEmpty(ctx)),
		slog.String("remote_addr", remote),
	)
}

// logFailure emits the audit event after a rejected authentication.
func (i *Interceptor) logFailure(ctx context.Context, keyID, reason, procedure, remote string) {
	slog.LogAttrs(ctx, slog.LevelWarn, "api_key_auth_failed",
		slog.Bool("audit", true),
		slog.String("reason", reason),
		slog.String("key_id", keyID),
		slog.String("method", procedure),
		slog.String("request_id", reqIDOrEmpty(ctx)),
		slog.String("remote_addr", remote),
	)
}

func reqIDOrEmpty(ctx context.Context) string {
	id, _ := reqid.FromContext(ctx)
	return id
}

// WrapUnary authenticates unary handler calls.
func (i *Interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		id, keyID, reason, err := i.authenticate(req.Header())
		procedure := req.Spec().Procedure
		remote := req.Peer().Addr
		if err != nil {
			i.logFailure(ctx, keyID, reason, procedure, remote)
			if i.metrics != nil {
				i.metrics.RecordAuth(ctx, "failed", reason)
			}
			return nil, err
		}
		i.logSuccess(ctx, id, keyID, procedure, remote)
		if i.metrics != nil {
			i.metrics.RecordAuth(ctx, "success", "")
		}
		return next(WithIdentity(ctx, id), req)
	}
}

// WrapStreamingClient is a pass-through; this interceptor is server-side only.
func (i *Interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler authenticates server-streaming handler calls (e.g.
// WatchSession).
func (i *Interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		id, keyID, reason, err := i.authenticate(conn.RequestHeader())
		procedure := conn.Spec().Procedure
		remote := conn.Peer().Addr
		if err != nil {
			i.logFailure(ctx, keyID, reason, procedure, remote)
			if i.metrics != nil {
				i.metrics.RecordAuth(ctx, "failed", reason)
			}
			return err
		}
		i.logSuccess(ctx, id, keyID, procedure, remote)
		if i.metrics != nil {
			i.metrics.RecordAuth(ctx, "success", "")
		}
		return next(WithIdentity(ctx, id), conn)
	}
}
