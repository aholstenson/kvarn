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
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/egress/proxy"
)

// staticDialer routes any DialContext to a fixed address regardless of the
// requested host:port. Mirrors what the netstack will do at runtime — DNS
// resolution happens in the VM, but the proxy ignores the provided IP and
// dials the test upstream we control.
type staticDialer struct{ target string }

func (s staticDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, s.target)
}

var _ = Describe("Allowlist", func() {
	It("matches exact and wildcard entries", func() {
		a := proxy.NewAllowlist([]string{
			"github.com",
			"*.example.com",
		})
		Expect(a.Permit("github.com")).To(BeTrue())
		Expect(a.Permit("GitHub.com")).To(BeTrue())
		Expect(a.Permit("api.github.com")).To(BeFalse())
		Expect(a.Permit("foo.example.com")).To(BeTrue())
		Expect(a.Permit("a.b.example.com")).To(BeTrue())
		Expect(a.Permit("example.com")).To(BeFalse())
		Expect(a.Permit("evil.com")).To(BeFalse())
	})

	It("strips port and trailing dot", func() {
		a := proxy.NewAllowlist([]string{"foo.com"})
		Expect(a.Permit("foo.com.")).To(BeTrue())
		Expect(a.Permit("foo.com:443")).To(BeTrue())
	})
})

var _ = Describe("CA", func() {
	It("issues leaf certs that chain to the CA", func() {
		ca, err := proxy.GenerateCA()
		Expect(err).NotTo(HaveOccurred())
		Expect(ca.CertPEM()).NotTo(BeEmpty())

		cert, err := ca.LeafCert("api.github.com")
		Expect(err).NotTo(HaveOccurred())
		Expect(cert.Certificate).To(HaveLen(2))

		pool := x509.NewCertPool()
		Expect(pool.AppendCertsFromPEM(ca.CertPEM())).To(BeTrue())

		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		Expect(err).NotTo(HaveOccurred())
		_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "api.github.com"})
		Expect(err).NotTo(HaveOccurred())
	})

	It("caches leaf certs by host", func() {
		ca, err := proxy.GenerateCA()
		Expect(err).NotTo(HaveOccurred())
		c1, _ := ca.LeafCert("a.example")
		c2, _ := ca.LeafCert("a.example")
		Expect(c1).To(BeIdenticalTo(c2))
	})
})

var _ = Describe("Proxy ServeHTTPS", func() {
	var (
		upstream    *httptest.Server
		gotAuth     atomic.Value
		gotPath     atomic.Value
		ca          *proxy.CA
		listener    net.Listener
		serveCancel context.CancelFunc
	)

	BeforeEach(func() {
		gotAuth.Store("")
		gotPath.Store("")
		upstream = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth.Store(r.Header.Get("Authorization"))
			gotPath.Store(r.URL.Path)
			io.WriteString(w, "ok")
		}))

		var err error
		ca, err = proxy.GenerateCA()
		Expect(err).NotTo(HaveOccurred())

		listener, err = net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())

		// Build a dialer that ignores the host:port and dials the test
		// upstream. This is exactly what the netstack will hand the proxy
		// at runtime: any host the VM resolves+connects to gets redirected
		// to our listener; the proxy chooses the upstream.
		upHost := strings.TrimPrefix(upstream.URL, "https://")

		p := proxy.New(proxy.Config{
			Allowlist: proxy.NewAllowlist([]string{"api.github.com"}),
			CA:        ca,
			Injector: proxy.InjectorFunc(func(req *http.Request, host string) error {
				req.Header.Set("Authorization", "Bearer kvarn-fake-"+host)
				return nil
			}),
			Dialer:      insecureUpstreamDialer{target: upHost},
			UpstreamTLS: &tls.Config{InsecureSkipVerify: true},
		})

		var ctx context.Context
		ctx, serveCancel = context.WithCancel(context.Background())
		go p.ServeHTTPS(ctx, listener)
	})

	AfterEach(func() {
		serveCancel()
		listener.Close()
		upstream.Close()
	})

	It("forwards allowlisted SNI and injects fake auth", func() {
		pool := x509.NewCertPool()
		Expect(pool.AppendCertsFromPEM(ca.CertPEM())).To(BeTrue())

		conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{
			ServerName: "api.github.com",
			RootCAs:    pool,
		})
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close()

		req, _ := http.NewRequest("GET", "https://api.github.com/zen", nil)
		Expect(req.Write(conn)).To(Succeed())

		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(buf[:n])).To(ContainSubstring("ok"))

		Expect(gotPath.Load()).To(Equal("/zen"))
		Expect(gotAuth.Load()).To(Equal("Bearer kvarn-fake-api.github.com"))
	})

	It("rejects non-allowlisted SNI", func() {
		pool := x509.NewCertPool()
		Expect(pool.AppendCertsFromPEM(ca.CertPEM())).To(BeTrue())

		conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{
			ServerName: "evil.example.com",
			RootCAs:    pool,
		})
		// Connection accepted but proxy must close after SNI inspection.
		if err == nil {
			defer conn.Close()
			one := make([]byte, 1)
			_, readErr := conn.Read(one)
			Expect(readErr).To(HaveOccurred())
		}
	})
})

// insecureUpstreamDialer dials the test upstream over TLS-skipping the
// outer real TLS handshake, since the proxy itself dials and wraps with
// TLS. We do this by handing the proxy a connection to httptest's
// host:port — but httptest.NewTLSServer terminates TLS on its own listener,
// so we accept that the proxy's upstream tls.Client.Handshake will need to
// trust the test server's CA. We expose a dialer that returns a conn that,
// when wrapped by the proxy in tls.Client, talks to the test server's TLS
// listener directly.
type insecureUpstreamDialer struct{ target string }

func (i insecureUpstreamDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, i.target)
}
