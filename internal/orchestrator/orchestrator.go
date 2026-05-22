package orchestrator

import (
	"log/slog"
	"net/http"

	"connectrpc.com/connect"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/orchestrator/auth"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func run(addr string, svcOpts ServiceOpts) error {
	svc := NewServiceWithOpts(svcOpts)

	mux := http.NewServeMux()

	// Authenticate the public OrchestratorService surface. The host-local
	// BridgeService below is intentionally left unauthenticated.
	var handlerOpts []connect.HandlerOption
	if svc.authEnabled {
		handlerOpts = append(handlerOpts, connect.WithInterceptors(auth.NewInterceptor(svc.apiKeyStore)))
	}

	path, handler := kvarnv1connect.NewOrchestratorServiceHandler(svc, handlerOpts...)
	mux.Handle(path, handler)

	bridgePath, bridgeHandler := kvarnv1connect.NewBridgeServiceHandler(svc.BridgeHandler())
	mux.Handle(bridgePath, bridgeHandler)

	slog.Info("orchestrator listening", "addr", addr)
	return http.ListenAndServe(addr, h2c.NewHandler(mux, &http2.Server{}))
}
