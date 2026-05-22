//go:build darwin || linux

package local

import (
	"context"
	"log/slog"

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

	return nil
}
