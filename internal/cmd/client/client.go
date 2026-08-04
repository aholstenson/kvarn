// Package client builds OrchestratorService clients for the CLI, over either
// the orchestrator's network listener or its host-local control socket.
package client

import (
	"context"
	"net/http"
	"os"

	"connectrpc.com/connect"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/localsock"
)

// DefaultAddr is the network listener a command falls back to when no address
// is given and no local socket is present.
const DefaultAddr = "http://localhost:8080"

// Resolve picks the transport a CLI command should use. An explicit address
// wins; otherwise the host-local socket when one exists, so a command run on
// the orchestrator's own host needs neither a flag nor a key; otherwise the
// default network address.
func Resolve(addr string) string {
	if addr != "" {
		return addr
	}
	sock := os.Getenv("KVARN_LOCAL_SOCKET")
	if sock == "" {
		sock = localsock.DefaultPath()
	}
	if localsock.Exists(sock) {
		return localsock.Address(sock)
	}
	return DefaultAddr
}

// NewOrchestrator returns an OrchestratorService client for addr, which is
// either an http(s) URL or "unix://<path>". When apiKey is non-empty, every
// unary and streaming request carries an "Authorization: Bearer <apiKey>"
// header; the socket needs no key, since the filesystem is what authenticates
// a caller there.
func NewOrchestrator(addr, apiKey string) kvarnv1connect.OrchestratorServiceClient {
	var opts []connect.ClientOption
	if apiKey != "" {
		opts = append(opts, connect.WithInterceptors(&bearerInterceptor{token: apiKey}))
	}

	httpClient := http.DefaultClient
	baseURL := addr
	if path, ok := localsock.Path(addr); ok {
		httpClient = localsock.HTTPClient(path)
		// The host in this URL is never resolved — the transport dials the
		// socket regardless — but connect-go still needs a syntactically valid
		// base URL to build request URLs from.
		baseURL = "http://kvarn-local"
	}
	return kvarnv1connect.NewOrchestratorServiceClient(httpClient, baseURL, opts...)
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
