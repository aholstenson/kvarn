// Package link runs a userspace TCP/IP stack per VM. The orchestrator owns
// the stack and the only network reachable from inside the VM; outbound
// connections are surfaced as net.Conn instances on the orchestrator side
// for the egress proxy to terminate, inspect, and forward.
//
// The actual ethernet frame transport between the netstack and the VM's
// virtual NIC is handled by hypervisor-specific endpoint adapters
// (endpoint_darwin.go for vz, endpoint_linux.go for qemu).
package link

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/cockroachdb/errors"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// Default addresses for the per-VM virtual network. The VM is handed out
// 10.0.2.2; the gateway (where the proxy and DNS forwarder listen) is
// 10.0.2.1. These follow the qemu user-mode network conventions so that
// behaviour is comparable to the previous setup.
const (
	GatewayIP = "10.0.2.1"
	VMIP      = "10.0.2.2"
	Subnet    = "10.0.2.0/24"
	Netmask   = "255.255.255.0"
	DefaultMTU = 1500
)

// Config configures a Network.
type Config struct {
	// Endpoint is the L2 transport between the netstack and the VM. The
	// caller is responsible for plumbing it to the hypervisor's NIC fd.
	Endpoint stack.LinkEndpoint

	// AllowedDNS is the set of hostnames the DNS forwarder will resolve.
	// Other names yield NXDOMAIN. Defense in depth on top of the proxy's
	// allowlist.
	AllowedDNS []string

	// Logger; defaults to slog.Default().
	Logger *slog.Logger
}

// Network is a per-VM userspace TCP/IP fabric. It owns a gvisor stack,
// listeners for the egress proxy on the gateway IP, and a DNS forwarder.
type Network struct {
	cfg  Config
	log  *slog.Logger
	stk  *stack.Stack
	nic  tcpip.NICID

	mu       sync.Mutex
	tcpListeners map[uint16]*gonet.TCPListener
	closed   bool

	cancel context.CancelFunc
}

// New constructs a Network and attaches the link endpoint to a fresh
// gvisor stack. It does not return until the stack is ready to accept
// traffic.
func New(cfg Config) (*Network, error) {
	if cfg.Endpoint == nil {
		return nil, errors.New("link.New: Endpoint is required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})

	const nicID tcpip.NICID = 1
	if err := s.CreateNIC(nicID, cfg.Endpoint); err != nil {
		return nil, errors.Newf("create NIC: %s", err)
	}

	// Spoofing + promiscuous: the stack accepts traffic for any destination
	// IP, not just our virtual gateway. This is what lets us pose as
	// arbitrary upstream IPs that the VM resolved through DNS.
	if err := s.SetSpoofing(nicID, true); err != nil {
		return nil, errors.Newf("set spoofing: %s", err)
	}
	if err := s.SetPromiscuousMode(nicID, true); err != nil {
		return nil, errors.Newf("set promiscuous: %s", err)
	}

	gw := tcpip.AddrFromSlice(net.ParseIP(GatewayIP).To4())
	addrWithPrefix := tcpip.AddressWithPrefix{Address: gw, PrefixLen: 24}
	protoAddr := tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: addrWithPrefix}
	if err := s.AddProtocolAddress(nicID, protoAddr, stack.AddressProperties{}); err != nil {
		return nil, errors.Newf("add gateway IP: %s", err)
	}

	// Default route: anything goes to our NIC; spoofing covers the rest.
	s.SetRouteTable([]tcpip.Route{{
		Destination: header.IPv4EmptySubnet,
		NIC:         nicID,
	}})

	n := &Network{
		cfg:          cfg,
		log:          log,
		stk:          s,
		nic:          nicID,
		tcpListeners: make(map[uint16]*gonet.TCPListener),
	}
	return n, nil
}

// Run starts background goroutines (DNS forwarder etc.) and blocks until
// ctx is cancelled or Close is called.
func (n *Network) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	n.cancel = cancel

	// DNS forwarder on gateway:53.
	dns, err := n.startDNS(ctx)
	if err != nil {
		return errors.Wrap(err, "start DNS")
	}
	defer dns.Close()

	<-ctx.Done()
	return nil
}

// Listen binds a TCP listener on the gateway IP at the given port. The
// proxy uses this for :80 and :443.
func (n *Network) Listen(port uint16) (net.Listener, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil, errors.New("network closed")
	}

	addr := tcpip.FullAddress{
		NIC:  n.nic,
		Addr: tcpip.AddrFromSlice(net.ParseIP(GatewayIP).To4()),
		Port: port,
	}
	ln, err := gonet.ListenTCP(n.stk, addr, ipv4.ProtocolNumber)
	if err != nil {
		return nil, errors.Wrap(err, "listen TCP")
	}
	n.tcpListeners[port] = ln
	return ln, nil
}

// ListenAny binds a TCP listener that accepts connections to any
// destination IP at the given port. The stack's spoofing+promiscuous mode
// makes this possible: SYNs aimed at, say, 140.82.114.4:443 are admitted
// and surfaced through this listener as if the gateway were the real
// destination.
func (n *Network) ListenAny(port uint16) (net.Listener, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil, errors.New("network closed")
	}

	// Address 0.0.0.0 plus spoofing on the NIC is the gvisor idiom for
	// "accept any destination".
	addr := tcpip.FullAddress{NIC: n.nic, Port: port}
	ln, err := gonet.ListenTCP(n.stk, addr, ipv4.ProtocolNumber)
	if err != nil {
		return nil, errors.Wrap(err, "listen TCP wildcard")
	}
	n.tcpListeners[port] = ln
	return ln, nil
}

// Close tears down listeners and the stack. Idempotent.
func (n *Network) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil
	}
	n.closed = true
	for _, ln := range n.tcpListeners {
		ln.Close()
	}
	if n.cancel != nil {
		n.cancel()
	}
	n.stk.Close()
	return nil
}

// Stack exposes the underlying gvisor stack for tests and advanced uses.
func (n *Network) Stack() *stack.Stack { return n.stk }

// startDNS starts a UDP forwarder on gateway:53 and returns a closer.
func (n *Network) startDNS(ctx context.Context) (interface{ Close() error }, error) {
	wq := &waiter.Queue{}
	ep, e := n.stk.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, wq)
	if e != nil {
		return nil, errors.Newf("new udp endpoint: %s", e)
	}
	addr := tcpip.FullAddress{
		NIC:  n.nic,
		Addr: tcpip.AddrFromSlice(net.ParseIP(GatewayIP).To4()),
		Port: 53,
	}
	if err := ep.Bind(addr); err != nil {
		ep.Close()
		return nil, errors.Newf("bind udp 53: %s", err)
	}
	conn := gonet.NewUDPConn(wq, ep)
	srv := &dnsForwarder{
		conn:    conn,
		allowed: n.cfg.AllowedDNS,
		log:     n.log,
	}
	go srv.run(ctx)
	return conn, nil
}

// String formats a tcpip.Address from text for diagnostics.
func ipString(a tcpip.Address) string {
	b := a.AsSlice()
	if len(b) != 4 {
		return fmt.Sprintf("%v", b)
	}
	return fmt.Sprintf("%d.%d.%d.%d", b[0], b[1], b[2], b[3])
}
