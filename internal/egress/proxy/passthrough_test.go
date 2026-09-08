package proxy_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/egress/proxy"
)

var _ = Describe("TLS passthrough", func() {
	var (
		upstream    *httptest.Server
		listener    net.Listener
		kvarnCA     *proxy.CA
		serveCancel context.CancelFunc
	)

	// serve starts a proxy on a local listener with one rule, which is what a
	// `tls:` entry in kvarn.yml becomes.
	serve := func(rule proxy.Rule) {
		p := proxy.New(proxy.Config{
			Allowlist:   proxy.NewAllowlist([]proxy.Rule{rule}),
			CA:          kvarnCA,
			Dialer:      staticDialer{target: strings.TrimPrefix(upstream.URL, "https://")},
			UpstreamTLS: &tls.Config{InsecureSkipVerify: true},
		})
		var ctx context.Context
		ctx, serveCancel = context.WithCancel(context.Background())
		go func() {
			defer GinkgoRecover()
			_ = p.ServeHTTPS(ctx, listener)
		}()
	}

	BeforeEach(func() {
		upstream = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "ok")
		}))
		DeferCleanup(upstream.Close)

		var err error
		kvarnCA, err = proxy.GenerateCA()
		Expect(err).NotTo(HaveOccurred())

		listener, err = net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(listener.Close)

		DeferCleanup(func() {
			if serveCancel != nil {
				serveCancel()
			}
		})
	})

	// kvarnPool trusts only kvarn's own CA, which is what the guest is given.
	kvarnPool := func() *x509.CertPool {
		pool := x509.NewCertPool()
		Expect(pool.AppendCertsFromPEM(kvarnCA.CertPEM())).To(BeTrue())
		return pool
	}

	It("hands the client the upstream's own certificate", func() {
		// This is the whole point: a client that pins a certificate has to see
		// the real one, so the handshake must not chain to kvarn's CA.
		serve(proxy.Rule{Host: "api.pinned.example.com", Passthrough: true})

		conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{
			ServerName:         "api.pinned.example.com",
			InsecureSkipVerify: true,
		})
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close()

		presented := conn.ConnectionState().PeerCertificates
		Expect(presented).NotTo(BeEmpty())
		_, err = presented[0].Verify(x509.VerifyOptions{Roots: kvarnPool()})
		Expect(err).To(HaveOccurred())
		Expect(presented[0].Equal(upstream.Certificate())).To(BeTrue())
	})

	It("still terminates a host left on the default inspect mode", func() {
		serve(proxy.Rule{Host: "api.github.com"})

		conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{
			ServerName: "api.github.com",
			RootCAs:    kvarnPool(),
		})
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close()

		Expect(conn.ConnectionState().PeerCertificates[0].Equal(upstream.Certificate())).To(BeFalse())
	})

	It("refuses a host that is not allowed at all, passthrough or not", func() {
		serve(proxy.Rule{Host: "api.pinned.example.com", Passthrough: true})

		conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{
			ServerName:         "evil.example.com",
			InsecureSkipVerify: true,
		})
		if err == nil {
			defer conn.Close()
			one := make([]byte, 1)
			_, readErr := conn.Read(one)
			Expect(readErr).To(HaveOccurred())
		}
	})
})
