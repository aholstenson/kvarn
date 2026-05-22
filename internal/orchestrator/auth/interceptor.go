package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/aholstenson/kvarn/internal/config/apikey"
)

const bearerPrefix = "Bearer "

// Interceptor authenticates external OrchestratorService requests against the
// API key store and injects the resulting Identity into the request context.
// It is only installed on the public handler; the host-local BridgeService is
// left unauthenticated.
type Interceptor struct {
	store apikey.Store
}

// NewInterceptor returns an Interceptor backed by store.
func NewInterceptor(store apikey.Store) *Interceptor {
	return &Interceptor{store: store}
}

// authenticate validates the Authorization header and returns the caller's
// Identity. Any failure to recognize the key (missing/malformed header,
// unknown key ID, wrong secret, disabled, expired) maps to Unauthenticated; a
// store/parse failure maps to Unavailable so the service fails closed.
func (i *Interceptor) authenticate(header http.Header) (*Identity, error) {
	authz := header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing or malformed Authorization header"))
	}

	keyID, secret, ok := apikey.ParseToken(strings.TrimPrefix(authz, bearerPrefix))
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("malformed token"))
	}

	key, err := i.store.Get(context.Background(), keyID)
	if err != nil {
		if errors.Is(err, apikey.ErrNotFound) {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unknown key"))
		}
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read api key store: %w", err))
	}

	if subtle.ConstantTimeCompare([]byte(apikey.HashSecret(secret)), []byte(key.Hash)) != 1 {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token"))
	}
	if key.Disabled {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("key disabled"))
	}
	if key.Expired(time.Now()) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("key expired"))
	}

	return &Identity{KeyName: key.Name, Projects: key.Projects}, nil
}

// WrapUnary authenticates unary handler calls.
func (i *Interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		id, err := i.authenticate(req.Header())
		if err != nil {
			return nil, err
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
		id, err := i.authenticate(conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(WithIdentity(ctx, id), conn)
	}
}
