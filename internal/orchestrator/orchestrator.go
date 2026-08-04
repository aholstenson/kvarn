package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/localsock"
	"github.com/aholstenson/kvarn/internal/observability/metrics"
	"github.com/aholstenson/kvarn/internal/observability/reqid"
	"github.com/aholstenson/kvarn/internal/orchestrator/auth"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// shutdownTimeout caps the wall-time spent draining the HTTP server and any
// in-flight jobs. The bounded VM-stop path keeps Sandbox.Close from running
// indefinitely, so this is the outer envelope around all of that.
const shutdownTimeout = 30 * time.Second

// serveOpts are the listeners run() should bring up.
type serveOpts struct {
	// Addr is the TCP address of the network listener.
	Addr string
	// LocalSocket is the path of the host-local control socket; empty disables
	// it, leaving the network listener as the only way in.
	LocalSocket string
}

func run(ctx context.Context, listen serveOpts, svcOpts ServiceOpts) error {
	svc := NewServiceWithOpts(svcOpts)
	svc.StartRepoMaintenance(ctx)

	srv := &http.Server{
		Addr:    listen.Addr,
		Handler: h2c.NewHandler(PublicMux(svc), &http2.Server{}),
	}

	// Both listeners serve the same OrchestratorService and differ only in the
	// interceptor that establishes the caller's identity, so a handler never
	// learns which one a request came in on. ConnContext is what lets the local
	// interceptor see the connection: it is the only place net/http exposes it,
	// and without it that interceptor fails closed.
	var (
		localSrv      *http.Server
		localListener net.Listener
	)
	if listen.LocalSocket != "" {
		l, err := localsock.Listen(listen.LocalSocket)
		if err != nil {
			return err
		}
		localListener = l
		localSrv = &http.Server{
			Handler:     h2c.NewHandler(LocalMux(svc), &http2.Server{}),
			ConnContext: auth.ConnContext,
		}
	}

	errCh := make(chan error, 2)
	go func() { errCh <- srv.ListenAndServe() }()
	slog.Info("orchestrator listening", "addr", listen.Addr)
	if localSrv != nil {
		go func() { errCh <- localSrv.Serve(localListener) }()
		slog.Info("host-local control socket listening", "path", listen.LocalSocket)
	}

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
	if localSrv != nil {
		// Closing the listener unlinks the socket file, so a restart finds no
		// stale path to reason about.
		if err := localSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("local socket shutdown returned error", "error", err)
		}
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
	var authInterceptor connect.Interceptor
	if svc.authEnabled {
		authInterceptor = auth.NewInterceptor(svc.apiKeyStore, auth.WithMetrics(svc.instruments))
	}
	return svc.mux(authInterceptor)
}

// LocalMux builds the HTTP mux for the host-local control socket. It serves the
// same OrchestratorService as PublicMux; only the identity source differs, so
// the socket is a second way to satisfy the authorization model rather than a
// second copy of it.
//
// The local interceptor is installed even with auth disabled. Its checks are
// no-ops then, but it is also what refuses a request that did not arrive over a
// unix socket, and that guard should not depend on a dev-mode flag.
func LocalMux(svc *Service) *http.ServeMux {
	return svc.mux(auth.NewLocalInterceptor(auth.WithLocalMetrics(svc.instruments)))
}

// mux mounts OrchestratorService behind the shared interceptor chain, with
// authInterceptor establishing the caller's identity. A nil authInterceptor
// leaves requests unauthenticated, which is what --no-auth means.
//
// Chain order matters: request-id first so auth audit logs and the RPC duration
// histogram both carry it; auth before the RPC timer keeps unauthenticated
// rejects out of the per-procedure latency distribution for authenticated
// traffic.
func (s *Service) mux(authInterceptor connect.Interceptor) *http.ServeMux {
	mux := http.NewServeMux()

	interceptors := []connect.Interceptor{reqid.NewInterceptor()}
	if authInterceptor != nil {
		interceptors = append(interceptors, authInterceptor)
	}
	if rpc, err := metrics.NewRPCInterceptor(s.meter); err == nil {
		interceptors = append(interceptors, rpc)
	} else {
		slog.Warn("rpc duration metric disabled", "error", err)
	}

	path, handler := kvarnv1connect.NewOrchestratorServiceHandler(s, connect.WithInterceptors(interceptors...))
	mux.Handle(path, handler)

	return mux
}
