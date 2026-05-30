package dispatch

import (
	"context"
	"log/slog"
	"net"
	"net/http"

	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// connContextKey is the context key under which the accepted net.Conn is
// stashed by http.Server.ConnContext, so the bridge handler can extract the
// peer's vsock CID for per-RPC binding checks.
type connContextKey struct{}

// ConnFromContext returns the net.Conn that carried the current HTTP request,
// if Serve was used to stand up the server.
func ConnFromContext(ctx context.Context) (net.Conn, bool) {
	conn, ok := ctx.Value(connContextKey{}).(net.Conn)
	return conn, ok
}

// Serve starts an h2c HTTP server on the given listener, serving the
// BridgeService using the provided handler. It blocks until the listener
// is closed or an error occurs.
func Serve(listener net.Listener, handler kvarnv1connect.BridgeServiceHandler) {
	mux := http.NewServeMux()
	path, h := kvarnv1connect.NewBridgeServiceHandler(handler)
	mux.Handle(path, h)

	srv := &http.Server{
		Handler: h2c.NewHandler(mux, &http2.Server{}),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, connContextKey{}, c)
		},
	}
	if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
		slog.Error("bridge server error", "error", err)
	}
}
