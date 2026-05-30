package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/orchestrator/auth"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// shutdownTimeout caps the wall-time spent draining the HTTP server and any
// in-flight jobs. The bounded VM-stop path keeps Sandbox.Close from running
// indefinitely, so this is the outer envelope around all of that.
const shutdownTimeout = 30 * time.Second

func run(ctx context.Context, addr string, svcOpts ServiceOpts) error {
	svc := NewServiceWithOpts(svcOpts)
	srv := &http.Server{
		Addr:    addr,
		Handler: h2c.NewHandler(PublicMux(svc), &http2.Server{}),
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	slog.Info("orchestrator listening", "addr", addr)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("http shutdown returned error", "error", err)
	}
	svc.Shutdown(shutdownCtx)
	return nil
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
