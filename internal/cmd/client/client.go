// Package client builds OrchestratorService clients for the CLI, optionally
// attaching an API-key bearer token to every request.
package client

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
)

// NewOrchestrator returns an OrchestratorService client for addr. When apiKey
// is non-empty, every unary and streaming request carries an
// "Authorization: Bearer <apiKey>" header.
func NewOrchestrator(addr, apiKey string) kvarnv1connect.OrchestratorServiceClient {
	var opts []connect.ClientOption
	if apiKey != "" {
		opts = append(opts, connect.WithInterceptors(&bearerInterceptor{token: apiKey}))
	}
	return kvarnv1connect.NewOrchestratorServiceClient(http.DefaultClient, addr, opts...)
}

// bearerInterceptor sets the Authorization header on outgoing client requests.
type bearerInterceptor struct {
	token string
}

func (b *bearerInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			req.Header().Set("Authorization", "Bearer "+b.token)
		}
		return next(ctx, req)
	}
}

func (b *bearerInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+b.token)
		return conn
	}
}

func (b *bearerInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
