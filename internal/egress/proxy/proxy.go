package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Dialer dials an upstream address. Override in tests.
type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

type netDialer struct{ d net.Dialer }

func (n netDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return n.d.DialContext(ctx, network, addr)
}

// Resolver answers which hostnames the VM was told an address belongs to. It
// is how a connection on a port that carries no hostname of its own — a
// database, a mail relay — is still judged by name: the guest asked the VM's
// own DNS forwarder for the address moments earlier, and that answer is the
// only honest link between the packet and the name in kvarn.yml.
type Resolver interface {
	// HostnamesFor returns the names answered with this address, newest first,
	// or nothing when the address was never handed out.
	HostnamesFor(ip string) []string
}

// Proxy is a host-side egress proxy. It accepts plaintext HTTP on one
// listener and TLS-CONNECT-style TLS traffic on another. SNI / Host header
// is consulted against the allowlist; allowed traffic is terminated, the
// SecretInjector runs, and the request is forwarded to the real upstream.
//
// Ports beyond 80 and 443 are served by ServeTCP, which carries the bytes
// through untouched — there is no HTTP on them to read, so there is nothing to
// inspect or inject.
type Proxy struct {
	allowlist   *Allowlist
	ca          *CA
	injector    SecretInjector
	dialer      Dialer
	upstreamTLS *tls.Config
	resolver    Resolver
	log         *slog.Logger
	onDenied    func(host string)
}

// Config configures a Proxy.
type Config struct {
	Allowlist   *Allowlist
	CA          *CA
	Injector    SecretInjector
	Dialer      Dialer
	UpstreamTLS *tls.Config // nil = system default; ServerName is set per-host
	// Resolver names the address a raw TCP connection is aimed at. ServeTCP
	// denies every connection without one, since it would otherwise have no
	// way to tell an allowed database from any other address on the internet.
	Resolver Resolver
	Logger   *slog.Logger
	// OnDenied, if set, is called with the hostname of every refused
	// connection. A refusal is a closed socket and nothing else, which reaches
	// the client as a truncated connection rather than as an explanation; this
	// is how the side that knows the reason gets to tell someone. It is called
	// from connection-handling goroutines, so it must be safe for concurrent
	// use and must not block.
	OnDenied func(host string)
}

// New constructs a Proxy. CA may be nil for HTTP-only proxies.
func New(cfg Config) *Proxy {
	d := cfg.Dialer
	if d == nil {
		d = netDialer{}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Proxy{
		allowlist:   cfg.Allowlist,
		ca:          cfg.CA,
		injector:    cfg.Injector,
		dialer:      d,
		upstreamTLS: cfg.UpstreamTLS,
		resolver:    cfg.Resolver,
		log:         log,
		onDenied:    cfg.OnDenied,
	}
}

// deny records a refused connection. The caller closes the connection; this
// only reports it.
func (p *Proxy) deny(host string) {
	p.log.Info("egress denied", "host", host)
	if p.onDenied != nil {
		p.onDenied(host)
	}
}

// ServeHTTPS reads ClientHello, terminates TLS using a per-host leaf cert
// signed by the CA, then proxies the inner HTTP request. The listener is
// expected to receive raw TCP/TLS traffic intercepted from the VM.
func (p *Proxy) ServeHTTPS(ctx context.Context, ln net.Listener) error {
	return p.serve(ctx, ln, p.handleTLS)
}

// ServeHTTP reads the Host header from a plaintext HTTP request and forwards.
func (p *Proxy) ServeHTTP(ctx context.Context, ln net.Listener) error {
	return p.serve(ctx, ln, p.handlePlain)
}

// ServeTCP carries connections on a port the proxy does not read HTTP on —
// a database, a message broker, a mail relay. The bytes pass through untouched
// in both directions.
//
// Nothing in the stream names its destination, so the allowlist is applied to
// the address the connection is aimed at, resolved back to the names the VM's
// DNS forwarder answered with it. An address the guest never looked up is
// denied: an IP literal typed straight into a connection string is exactly the
// case the allowlist exists to catch.
func (p *Proxy) ServeTCP(ctx context.Context, ln net.Listener, port uint16) error {
	return p.serve(ctx, ln, func(ctx context.Context, conn net.Conn) {
		p.handleTCP(ctx, conn, port)
	})
}

func (p *Proxy) handleTCP(ctx context.Context, conn net.Conn, port uint16) {
	defer conn.Close()

	// The netstack accepts these on a wildcard listener, so the local address
	// is the destination the guest aimed at rather than an address of ours.
	dest, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		p.log.Debug("tcp destination unreadable", "port", port, "error", err)
		return
	}

	// Every name the address answers to, and the address itself. Listing an IP
	// directly is a deliberate act, so it is honoured here the same way a name
	// is; what the resolver guards against is an address nobody declared.
	var candidates []string
	if p.resolver != nil {
		candidates = p.resolver.HostnamesFor(dest)
	}
	candidates = append(candidates, dest)

	host := ""
	for _, name := range candidates {
		if p.allowlist.Permit(name, port) {
			host = name
			break
		}
	}
	if host == "" {
		// Report the first name the address is known by, since that is the one
		// the reader would have to add. With no name, the address itself is all
		// there is to report — and the only thing that would allow it.
		p.deny(candidates[0])
		return
	}

	// Dial the address the guest resolved, not the name: the two could differ
	// if the record changed in between, and the allowlist decision was made
	// about this address.
	upstream, err := p.dialer.DialContext(ctx, "tcp", net.JoinHostPort(dest, strconv.Itoa(int(port))))
	if err != nil {
		p.log.Info("upstream dial failed", "host", host, "port", port, "error", err)
		return
	}
	defer upstream.Close()

	p.log.Debug("egress tcp", "host", host, "port", port)
	splice(conn, upstream)
}

// splice copies bytes both ways until either side is done, then unblocks the
// other by closing both. Neither end is a request the proxy understands, so
// there is nothing to do but carry them.
func splice(client, upstream net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, client)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		done <- struct{}{}
	}()
	<-done
}

func (p *Proxy) serve(ctx context.Context, ln net.Listener, handle func(context.Context, net.Conn)) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go handle(ctx, conn)
	}
}

func (p *Proxy) handleTLS(ctx context.Context, raw net.Conn) {
	defer raw.Close()

	raw.SetReadDeadline(time.Now().Add(10 * time.Second))

	peeked := newPeekConn(raw)
	host, err := peekSNI(peeked)
	if err != nil {
		p.log.Debug("peek SNI failed", "error", err)
		return
	}
	raw.SetReadDeadline(time.Time{})

	rule, ok := p.allowlist.Lookup(host, 443)
	if !ok {
		p.deny(host)
		return
	}

	// A host named for passthrough is one that inspection breaks: it pins the
	// certificate the client must see, or wants a client certificate the proxy
	// cannot present. Splice the bytes and let the two ends do their own
	// handshake. No secret can be injected into traffic nobody reads.
	if rule.Passthrough {
		upstream, err := p.dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, "443"))
		if err != nil {
			p.log.Info("upstream dial failed", "host", host, "error", err)
			return
		}
		defer upstream.Close()
		p.log.Debug("egress passthrough", "host", host, "port", 443)
		splice(peeked, upstream)
		return
	}

	leaf, err := p.ca.LeafCert(host)
	if err != nil {
		p.log.Error("leaf cert", "host", host, "error", err)
		return
	}

	tlsConn := tls.Server(peeked, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
	})
	defer tlsConn.Close()

	hsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	if err := tlsConn.HandshakeContext(hsCtx); err != nil {
		cancel()
		p.log.Debug("TLS handshake", "host", host, "error", err)
		return
	}
	cancel()

	upstream, err := p.dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, "443"))
	if err != nil {
		p.log.Info("upstream dial failed", "host", host, "error", err)
		return
	}
	defer upstream.Close()

	upCfg := p.upstreamTLS.Clone()
	if upCfg == nil {
		upCfg = &tls.Config{}
	}
	upCfg.ServerName = host
	upstreamTLS := tls.Client(upstream, upCfg)
	if err := upstreamTLS.HandshakeContext(ctx); err != nil {
		p.log.Info("upstream TLS handshake", "host", host, "error", err)
		return
	}
	defer upstreamTLS.Close()

	p.proxyHTTP(ctx, tlsConn, upstreamTLS, host, true)
}

func (p *Proxy) handlePlain(ctx context.Context, raw net.Conn) {
	defer raw.Close()
	br := bufio.NewReader(raw)

	raw.SetReadDeadline(time.Now().Add(10 * time.Second))
	req, err := http.ReadRequest(br)
	raw.SetReadDeadline(time.Time{})
	if err != nil {
		return
	}
	host := stripPort(req.Host)
	if host == "" {
		host = stripPort(req.URL.Host)
	}
	if !p.allowlist.Permit(host, 80) {
		p.deny(host)
		writeForbidden(raw)
		return
	}

	if err := p.injectAndForward(ctx, req, raw, host, false); err != nil {
		p.log.Debug("forward http", "host", host, "error", err)
	}
}

// proxyHTTP repeatedly reads HTTP requests off the inner conn (already
// terminated TLS or plain) and forwards them upstream, applying the secret
// injector before each forward.
func (p *Proxy) proxyHTTP(ctx context.Context, client net.Conn, upstream net.Conn, host string, isTLS bool) {
	br := bufio.NewReader(client)
	bw := bufio.NewWriter(client)
	upBr := bufio.NewReader(upstream)
	upBw := bufio.NewWriter(upstream)

	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		req.URL.Scheme = "https"
		if !isTLS {
			req.URL.Scheme = "http"
		}
		req.URL.Host = host
		req.RequestURI = ""

		if p.injector != nil {
			if err := p.injector.Inject(req, host); err != nil {
				p.log.Debug("injector failed", "host", host, "error", err)
				return
			}
		}

		// Re-encode for the upstream connection so injected headers land on
		// the wire. Use Write (not WriteProxy) since we already terminated.
		if err := writeRequest(req, upBw); err != nil {
			return
		}
		if err := upBw.Flush(); err != nil {
			return
		}

		resp, err := http.ReadResponse(upBr, req)
		if err != nil {
			return
		}
		if err := resp.Write(bw); err != nil {
			resp.Body.Close()
			return
		}
		resp.Body.Close()
		if err := bw.Flush(); err != nil {
			return
		}

		if req.Close || resp.Close || strings.EqualFold(resp.Header.Get("Connection"), "close") {
			return
		}
	}
}

func (p *Proxy) injectAndForward(ctx context.Context, req *http.Request, raw net.Conn, host string, isTLS bool) error {
	upstream, err := p.dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, "80"))
	if err != nil {
		return err
	}
	defer upstream.Close()

	if p.injector != nil {
		if err := p.injector.Inject(req, host); err != nil {
			return err
		}
	}

	req.URL.Scheme = "http"
	req.URL.Host = host
	req.RequestURI = ""

	upBw := bufio.NewWriter(upstream)
	if err := writeRequest(req, upBw); err != nil {
		return err
	}
	if err := upBw.Flush(); err != nil {
		return err
	}

	upBr := bufio.NewReader(upstream)
	resp, err := http.ReadResponse(upBr, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	bw := bufio.NewWriter(raw)
	if err := resp.Write(bw); err != nil {
		return err
	}
	return bw.Flush()
}

func writeRequest(req *http.Request, w io.Writer) error {
	// Use req.Write so headers (including any our injector added) are
	// emitted. Body re-use is fine because http.ReadRequest produced a
	// fresh body reader.
	return req.Write(w)
}

func writeForbidden(c net.Conn) {
	io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
}

func stripPort(host string) string {
	if i := strings.LastIndex(host, ":"); i >= 0 {
		return host[:i]
	}
	return host
}

// peekConn lets the SNI peeker re-deliver the bytes it consumed.
type peekConn struct {
	net.Conn
	buf *bufio.Reader
	mu  sync.Mutex
}

func newPeekConn(c net.Conn) *peekConn {
	// 32 KB is comfortably larger than a TLS record (max ~16 KB) so that
	// Peek can buffer an entire ClientHello before SNI parsing.
	return &peekConn{Conn: c, buf: bufio.NewReaderSize(c, 32*1024)}
}

func (p *peekConn) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.Read(b)
}

func (p *peekConn) Peek(n int) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.Peek(n)
}
