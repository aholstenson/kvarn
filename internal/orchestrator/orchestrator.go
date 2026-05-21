package orchestrator

import (
	"log/slog"
	"net/http"

	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func run(addr string, svcOpts ServiceOpts) error {
	svc := NewServiceWithOpts(svcOpts)

	mux := http.NewServeMux()

	path, handler := kvarnv1connect.NewOrchestratorServiceHandler(svc)
	mux.Handle(path, handler)

	bridgePath, bridgeHandler := kvarnv1connect.NewBridgeServiceHandler(svc.BridgeHandler())
	mux.Handle(bridgePath, bridgeHandler)

	slog.Info("orchestrator listening", "addr", addr)
	return http.ListenAndServe(addr, h2c.NewHandler(mux, &http2.Server{}))
}
