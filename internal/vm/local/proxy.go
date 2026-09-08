//go:build darwin || linux

package local

import (
	"context"
	"errors"
	"log/slog"
	"net"
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
	rules := egressproxy.HostRules(egressproxy.DefaultAllowedHosts)
	rules = append(rules, cfg.AllowedHosts...)

	allowlist := egressproxy.NewAllowlist(rules)
	p := egressproxy.New(egressproxy.Config{
		Allowlist: allowlist,
		CA:        ca,
		Injector:  cfg.SecretInjector,
		// The netstack's DNS forwarder is what named every address the guest
		// holds, so it is what can name a connection on a port with no
		// hostname in it.
		Resolver: n,
		Logger:   slog.Default(),
		OnDenied: cfg.OnEgressDenied,
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

	// The gateway caches bind before the project's own ports, because a guest
	// that cannot reach them installs nothing at all.
	reserved := map[uint16]string{}
	if cfg.ImageCacheHandler != nil && cfg.ImageCachePort != 0 {
		if err := startGatewayHTTP(ctx, n, "image cache", cfg.ImageCacheHandler, cfg.ImageCachePort); err != nil {
			return fmt.Errorf("start image cache: %w", err)
		}
		reserved[cfg.ImageCachePort] = "the container image cache"
	}
	if cfg.NixCacheHandler != nil && cfg.NixCachePort != 0 {
		if err := startGatewayHTTP(ctx, n, "nix cache", cfg.NixCacheHandler, cfg.NixCachePort); err != nil {
			return fmt.Errorf("start nix cache: %w", err)
		}
		reserved[cfg.NixCachePort] = "the Nix binary cache"
	}

	// One listener per port the project opened beyond HTTP. Binding them here,
	// rather than on demand, is what makes a connection to an unopened port
	// fail immediately instead of hanging: nothing is listening, so the
	// netstack answers the SYN itself.
	for _, port := range allowlist.ExtraPorts() {
		// A port the gateway already serves cannot also carry egress, and
		// saying so beats a bind error nobody can trace back to kvarn.yml.
		if owner, taken := reserved[port]; taken {
			return fmt.Errorf(
				"network.allowed_hosts opens port %d, which this host serves %s on; move that cache to another port or drop the entry",
				port, owner)
		}
		ln, err := n.ListenAny(port)
		if err != nil {
			return fmt.Errorf("listen %d: %w", port, err)
		}
		go func(ln net.Listener, port uint16) { _ = p.ServeTCP(ctx, ln, port) }(ln, port)
	}

	return nil
}

// startGatewayHTTP binds a shared HTTP handler to the per-VM gateway IP at
// port so a client inside the VM can reach it at a fixed address: podman via
// its mirror config, Nix via its substituter list. Each VM gets its own
// listener (one per gvisor netstack), but all listeners route to the same
// handler and on-disk store.
func startGatewayHTTP(ctx context.Context, n *link.Network, name string, handler http.Handler, port uint16) error {
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
			slog.Debug(name+" server stopped", "error", err)
		}
	}()
	return nil
}
