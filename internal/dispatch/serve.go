package dispatch

import (
	"log/slog"
	"net"
	"net/http"

	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// Serve starts an h2c HTTP server on the given listener, serving the
// BridgeService using the provided handler. It blocks until the listener
// is closed or an error occurs.
func Serve(listener net.Listener, handler kvarnv1connect.BridgeServiceHandler) {
	mux := http.NewServeMux()
	path, h := kvarnv1connect.NewBridgeServiceHandler(handler)
	mux.Handle(path, h)

	srv := &http.Server{
		Handler: h2c.NewHandler(mux, &http2.Server{}),
	}
	if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
		slog.Error("bridge server error", "error", err)
	}
}
