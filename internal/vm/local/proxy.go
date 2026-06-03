//go:build darwin || linux

package local

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"fmt"

	"github.com/aholstenson/kvarn/internal/egress/link"
	egressproxy "github.com/aholstenson/kvarn/internal/egress/proxy"
	"github.com/aholstenson/kvarn/internal/vm"
)

// startProxy binds HTTP and HTTPS listeners on the per-VM gateway IP and
// starts proxy goroutines bound to those listeners. The listeners outlive
// only as long as ctx; cancelling it tears them down.
func startProxy(ctx context.Context, n *link.Network, ca *egressproxy.CA, cfg vm.NetworkConfig) error {
	hosts := append([]string(nil), egressproxy.DefaultAllowedHosts...)
	hosts = append(hosts, cfg.AllowedHosts...)

	allowlist := egressproxy.NewAllowlist(hosts)
	p := egressproxy.New(egressproxy.Config{
		Allowlist: allowlist,
		CA:        ca,
		Injector:  cfg.SecretInjector,
		Logger:    slog.Default(),
	})

	httpsLn, err := n.ListenAny(443)
	if err != nil {
		return fmt.Errorf("listen 443: %w", err)
	}
	httpLn, err := n.ListenAny(80)
	if err != nil {
		httpsLn.Close()
		return fmt.Errorf("listen 80: %w", err)
	}

	go func() { _ = p.ServeHTTPS(ctx, httpsLn) }()
	go func() { _ = p.ServeHTTP(ctx, httpLn) }()

	if cfg.ImageCacheHandler != nil && cfg.ImageCachePort != 0 {
		if err := startImageCache(ctx, n, cfg.ImageCacheHandler, cfg.ImageCachePort); err != nil {
			return fmt.Errorf("start image cache: %w", err)
		}
	}

	return nil
}

// startImageCache binds the shared image-cache HTTP handler to the per-VM
// gateway IP at port so podman inside the VM can reach it via its mirror
// config. Each VM gets its own listener (one per gvisor netstack), but all
// listeners route to the same handler and on-disk store.
func startImageCache(ctx context.Context, n *link.Network, handler http.Handler, port uint16) error {
	ln, err := n.Listen(port)
	if err != nil {
		return fmt.Errorf("listen %d: %w", port, err)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Debug("image cache server stopped", "error", err)
		}
	}()
	return nil
}
