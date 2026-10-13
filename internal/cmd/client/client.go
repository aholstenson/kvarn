// Package client builds OrchestratorService clients for the CLI, over either
// the orchestrator's network listener or its host-local control socket.
package client

import (
	"context"
	"net/http"
	"os"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
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
	var interceptors []connect.ClientInterceptor
	if apiKey != "" {
		interceptors = append(interceptors, bearerInterceptor(apiKey))
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
	return kvarnv1connect.NewOrchestratorServiceClient(connect.NewClient(
		connecthttp.NewTransport(httpClient, baseURL, connecthttp.WithReadMaxBytes(0)),
		interceptors...,
	))
}

// bearerInterceptor sets the Authorization header on outgoing client requests.
func bearerInterceptor(token string) connect.ClientInterceptor {
	return func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			// The client attaches a CallInfo before the interceptor chain runs,
			// and the headers are only flushed on the first Send, so setting
			// them here reaches the wire.
			if info, ok := connect.CallInfoForClientContext(ctx); ok {
				info.RequestHeader().Set("Authorization", "Bearer "+token)
			}
			return next(ctx, spec)
		}
	}
}
