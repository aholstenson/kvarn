package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/aholstenson/kvarn/internal/config/apikey"
	"github.com/aholstenson/kvarn/internal/config/tomlstore"
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

// NewInterceptor returns a server interceptor backed by store. It
// authenticates unary and streaming calls (e.g. WatchSession) alike.
func NewInterceptor(store apikey.Store, opts ...Option) connect.ServerInterceptor {
	i := &Interceptor{store: store}
	for _, opt := range opts {
		opt(i)
	}
	return i.wrap
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
func (i *Interceptor) authenticate(header *connect.Header) (*Identity, string, string, error) {
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
		if errors.Is(err, tomlstore.ErrNotFound) {
			return nil, keyID, reasonUnknownKey, errAuthFailed
		}
		return nil, keyID, reasonStoreError,
			connect.NewError(connect.CodeUnavailable, fmt.Errorf("read api key store: %w", err).Error())
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

	return &Identity{
		Source:       SourceAPIKey,
		KeyID:        keyID,
		KeyName:      key.Name,
		Projects:     key.Projects,
		Capabilities: key.Capabilities,
	}, keyID, "", nil
}

// errAuthFailed is the single opaque error returned for every authentication
// failure to avoid leaking which check rejected the request.
var errAuthFailed = connect.NewError(connect.CodeUnauthenticated, "authentication failed")

// logSuccess emits the audit event after a successful authentication.
func (i *Interceptor) logSuccess(ctx context.Context, id *Identity, procedure, remote string) {
	slog.LogAttrs(ctx, slog.LevelInfo, "api_key_used", AuditAttrs(ctx, id, procedure, remote)...)
}

// AuditAttrs builds the attribute set every authentication audit line carries.
// Both interceptors go through it so a keyed caller and a local one are
// described the same way — one audit query answers "who did this" regardless of
// which listener the request arrived on.
func AuditAttrs(ctx context.Context, id *Identity, procedure, remote string) []slog.Attr {
	return []slog.Attr{
		slog.Bool("audit", true),
		slog.String("auth_source", id.Source),
		slog.String("key_name", id.KeyName),
		slog.String("key_id", id.KeyID),
		slog.String("peer", id.Peer),
		slog.String("method", procedure),
		slog.String("request_id", reqIDOrEmpty(ctx)),
		slog.String("remote_addr", remote),
	}
}

// logFailure emits the audit event after a rejected authentication.
func (i *Interceptor) logFailure(ctx context.Context, keyID, reason, procedure, remote string) {
	slog.LogAttrs(ctx, slog.LevelWarn, "api_key_auth_failed",
		slog.Bool("audit", true),
		slog.String("auth_source", SourceAPIKey),
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

// wrap authenticates every handler call before it reaches next.
func (i *Interceptor) wrap(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		var header *connect.Header
		var remote string
		if info, ok := connect.CallInfoForServerContext(ctx); ok {
			header = info.RequestHeader()
			remote = info.PeerAddr
		}
		id, keyID, reason, err := i.authenticate(header)
		if err != nil {
			i.logFailure(ctx, keyID, reason, spec.Procedure, remote)
			if i.metrics != nil {
				i.metrics.RecordAuth(ctx, "failed", reason)
			}
			return err
		}
		i.logSuccess(ctx, id, spec.Procedure, remote)
		if i.metrics != nil {
			i.metrics.RecordAuth(ctx, "success", "")
		}
		return next(WithIdentity(ctx, id), spec, stream)
	}
}
