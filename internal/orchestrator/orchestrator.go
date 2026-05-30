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
	mux := PublicMux(svc)

	slog.Info("orchestrator listening", "addr", addr)
	return http.ListenAndServe(addr, h2c.NewHandler(mux, &http2.Server{}))
}

// PublicMux builds the HTTP mux for the orchestrator's network listener. Only
// the authenticated OrchestratorService is exposed; BridgeService is served
// per-sandbox on the runner-only vsock transport in internal/sandbox and must
// not leak onto this mux — exposing it would publish an unauthenticated
// runner-impersonation entry point.
func PublicMux(svc *Service) *http.ServeMux {
	mux := http.NewServeMux()

	var handlerOpts []connect.HandlerOption
	if svc.authEnabled {
		handlerOpts = append(handlerOpts, connect.WithInterceptors(auth.NewInterceptor(svc.apiKeyStore)))
	}

	path, handler := kvarnv1connect.NewOrchestratorServiceHandler(svc, handlerOpts...)
	mux.Handle(path, handler)

	return mux
}
