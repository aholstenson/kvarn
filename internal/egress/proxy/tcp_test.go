package proxy_test

import (
	"context"
	"io"
	"net"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/egress/proxy"
)

// fakeResolver stands in for the VM's DNS forwarder: it answers which names an
// address was handed to the guest for.
type fakeResolver map[string][]string

func (f fakeResolver) HostnamesFor(ip string) []string { return f[ip] }

// echoServer accepts one connection and echoes every byte back, which is enough
// to prove a raw port was carried through in both directions.
func echoServer() (net.Listener, string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	go func() {
		defer GinkgoRecover()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(conn, conn); conn.Close() }()
		}
	}()
	return ln, ln.Addr().String()
}

var _ = Describe("ServeTCP", func() {
	const port = uint16(5432)

	var (
		upstreamAddr string
		client       net.Listener
		deniedHost   chan string
		serveCancel  context.CancelFunc
	)

	// start runs a proxy over a local listener. Connections arriving on it
	// stand in for what the netstack hands over: the address the guest aimed
	// at, which the test controls by dialling a listener bound to it.
	start := func(rules []proxy.Rule, resolver proxy.Resolver) {
		p := proxy.New(proxy.Config{
			Allowlist: proxy.NewAllowlist(rules),
			Resolver:  resolver,
			Dialer:    staticDialer{target: upstreamAddr},
			OnDenied:  func(host string) { deniedHost <- host },
		})
		ctx, cancel := context.WithCancel(context.Background())
		serveCancel = cancel
		go func() {
			defer GinkgoRecover()
			_ = p.ServeTCP(ctx, client, port)
		}()
	}

	BeforeEach(func() {
		deniedHost = make(chan string, 4)
		upstream, addr := echoServer()
		upstreamAddr = addr
		DeferCleanup(upstream.Close)

		var err error
		client, err = net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(client.Close)

		DeferCleanup(func() {
			if serveCancel != nil {
				serveCancel()
			}
		})
	})

	// dest is the address the proxy sees as the connection's destination. The
	// listener is on loopback, so that is what the resolver has to name.
	destOf := func(ln net.Listener) string {
		host, _, err := net.SplitHostPort(ln.Addr().String())
		Expect(err).NotTo(HaveOccurred())
		return host
	}

	It("carries a connection whose address resolves to an allowed host", func() {
		start(
			[]proxy.Rule{{Host: "db.example.com", Ports: []uint16{port}}},
			fakeResolver{destOf(client): {"db.example.com"}},
		)

		conn, err := net.Dial("tcp", client.Addr().String())
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close()

		_, err = conn.Write([]byte("SELECT 1"))
		Expect(err).NotTo(HaveOccurred())

		buf := make([]byte, 8)
		Expect(conn.SetReadDeadline(time.Now().Add(5 * time.Second))).To(Succeed())
		_, err = io.ReadFull(conn, buf)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(buf)).To(Equal("SELECT 1"))
	})

	It("denies an address the guest never resolved", func() {
		// An IP literal typed straight into a connection string reaches the
		// proxy with no name behind it, which is the case the allowlist exists
		// to catch.
		start(
			[]proxy.Rule{{Host: "db.example.com", Ports: []uint16{port}}},
			fakeResolver{},
		)

		conn, err := net.Dial("tcp", client.Addr().String())
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close()

		Eventually(deniedHost).Should(Receive(Equal(destOf(client))))
	})

	It("denies a resolved host that is not allowed on this port", func() {
		start(
			[]proxy.Rule{{Host: "db.example.com", Ports: []uint16{6379}}},
			fakeResolver{destOf(client): {"db.example.com"}},
		)

		conn, err := net.Dial("tcp", client.Addr().String())
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close()

		Eventually(deniedHost).Should(Receive(Equal("db.example.com")))
	})

	It("allows an address whose second name is the allowed one", func() {
		// One address answers for many names on shared hosting, and only one of
		// them has to be allowed.
		start(
			[]proxy.Rule{{Host: "db.example.com", Ports: []uint16{port}}},
			fakeResolver{destOf(client): {"cdn.example.net", "db.example.com"}},
		)

		conn, err := net.Dial("tcp", client.Addr().String())
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close()

		_, err = conn.Write([]byte("ping"))
		Expect(err).NotTo(HaveOccurred())

		buf := make([]byte, 4)
		Expect(conn.SetReadDeadline(time.Now().Add(5 * time.Second))).To(Succeed())
		_, err = io.ReadFull(conn, buf)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(buf)).To(Equal("ping"))
		Expect(deniedHost).NotTo(Receive())
	})

	It("denies every connection when nothing can name the address", func() {
		start(
			[]proxy.Rule{{Host: "db.example.com", Ports: []uint16{port}}},
			nil,
		)

		conn, err := net.Dial("tcp", client.Addr().String())
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close()

		Eventually(deniedHost).Should(Receive())
	})
})

var _ = Describe("ServeTCP with an address listed directly", func() {
	const port = uint16(6379)

	It("carries a connection to an IP the allowlist names outright", func() {
		// Listing an address is a deliberate act, so it is honoured the same
		// way a hostname is. What the resolver guards against is an address
		// nobody declared at all.
		upstream, upAddr := echoServer()
		DeferCleanup(upstream.Close)

		client, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(client.Close)

		p := proxy.New(proxy.Config{
			Allowlist: proxy.NewAllowlist([]proxy.Rule{{Host: "127.0.0.1", Ports: []uint16{port}}}),
			Resolver:  fakeResolver{},
			Dialer:    staticDialer{target: upAddr},
		})
		ctx, cancel := context.WithCancel(context.Background())
		DeferCleanup(cancel)
		go func() {
			defer GinkgoRecover()
			_ = p.ServeTCP(ctx, client, port)
		}()

		conn, err := net.Dial("tcp", client.Addr().String())
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close()

		_, err = conn.Write([]byte("PING"))
		Expect(err).NotTo(HaveOccurred())

		buf := make([]byte, 4)
		Expect(conn.SetReadDeadline(time.Now().Add(5 * time.Second))).To(Succeed())
		_, err = io.ReadFull(conn, buf)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(buf)).To(Equal("PING"))
	})
})
