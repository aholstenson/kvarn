package preview

import (
	"bytes"
	"context"
	"fmt"
	"net"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/project"
)

// vhostSites is the shape a base domain is for: two names answered by one
// server on one guest port.
func vhostSites(port uint16) *project.Config {
	return &project.Config{Preview: project.Preview{
		Sites: map[string]project.PreviewSite{
			"web": {Port: port, Host: "{ref}.{domain}"},
			"api": {Port: port, Host: "api-{ref}.{domain}"},
		},
	}}
}

// noResolve is a context whose deadline has already passed, which skips the
// hostname lookups. None of the names these specs use exist, and what they
// resolve to only decides whether the /etc/hosts hint is printed — so waiting
// on a real resolver would make the suite slow and its outcome depend on
// whatever the developer's machine happens to answer.
func noResolve() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

var _ = Describe("planSites with a base domain", func() {
	var cmd *Cmd

	BeforeEach(func() {
		cmd = &Cmd{BaseDomain: "sws.local", Ref: DefaultRefLabel}
	})

	It("gives every site the hostname its pattern resolves to", func() {
		plan, err := cmd.planSites(noResolve(), vhostSites(0))
		Expect(err).NotTo(HaveOccurred())
		defer plan.closeListeners()

		Expect(plan.sites).To(HaveLen(2))
		Expect(plan.sites[0].Name).To(Equal("api"))
		Expect(plan.sites[0].Host).To(Equal("api-local.sws.local"))
		Expect(plan.sites[1].Host).To(Equal("local.sws.local"))
		for _, site := range plan.sites {
			Expect(site.URL).To(Equal(siteURL(site.Host, plan.ingressPort)))
		}
	})

	It("serves every site from one listener", func() {
		plan, err := cmd.planSites(noResolve(), vhostSites(0))
		Expect(err).NotTo(HaveOccurred())
		defer plan.closeListeners()

		Expect(plan.ingress).NotTo(BeNil())
		for _, site := range plan.sites {
			Expect(site.Listener).To(BeNil())
		}
	})

	It("borrows the port the sites share inside the VM, so the URL means the same on both sides", func() {
		port, ln := takePort()
		Expect(ln.Close()).To(Succeed())

		plan, err := cmd.planSites(noResolve(), vhostSites(port))
		Expect(err).NotTo(HaveOccurred())
		defer plan.closeListeners()

		Expect(plan.ingressPort).To(Equal(port))
		Expect(plan.mismatchedPorts()).To(BeEmpty())
	})

	It("serves somewhere else when the sites do not agree on one port", func() {
		cfg := &project.Config{Preview: project.Preview{
			Sites: map[string]project.PreviewSite{
				"web": {Port: 3000, Host: "{ref}.{domain}"},
				"api": {Port: 4000, Host: "api-{ref}.{domain}"},
			},
		}}
		plan, err := cmd.planSites(noResolve(), cfg)
		Expect(err).NotTo(HaveOccurred())
		defer plan.closeListeners()

		Expect(plan.ingressPort).NotTo(BeZero())
		Expect(plan.mismatchedPorts()).To(ContainElements("api", "web"))
	})

	It("honours --ingress-port", func() {
		port, ln := takePort()
		Expect(ln.Close()).To(Succeed())

		cmd.IngressPort = port
		plan, err := cmd.planSites(noResolve(), vhostSites(3000))
		Expect(err).NotTo(HaveOccurred())
		defer plan.closeListeners()

		Expect(plan.ingressPort).To(Equal(port))
		Expect(plan.sites[0].URL).To(HaveSuffix(fmt.Sprintf(":%d", port)))
	})

	It("refuses an --ingress-port that is already in use rather than moving the preview", func() {
		port, ln := takePort()
		defer ln.Close()

		cmd.IngressPort = port
		_, err := cmd.planSites(noResolve(), vhostSites(3000))
		Expect(err).To(MatchError(ContainSubstring(fmt.Sprintf("bind ingress port %d", port))))
	})

	It("leaves the port off a URL served on the default HTTP port", func() {
		Expect(siteURL("local.sws.local", 80)).To(Equal("http://local.sws.local"))
		Expect(siteURL("local.sws.local", 8080)).To(Equal("http://local.sws.local:8080"))
	})

	It("refuses two sites that resolve to one hostname", func() {
		cfg := &project.Config{Preview: project.Preview{
			Sites: map[string]project.PreviewSite{
				"web": {Port: 3000},
				"api": {Port: 4000},
			},
		}}
		_, err := cmd.planSites(noResolve(), cfg)
		Expect(err).To(MatchError(ContainSubstring("both resolve to local.sws.local")))
	})

	It("refuses a host pattern that leaves the base domain", func() {
		cfg := &project.Config{Preview: project.Preview{
			Sites: map[string]project.PreviewSite{"web": {Port: 3000, Host: "admin.example.com"}},
		}}
		_, err := cmd.planSites(noResolve(), cfg)
		Expect(err).To(MatchError(ContainSubstring("outside the configured preview domain")))
	})

	It("refuses --port alongside --base-domain", func() {
		cmd.Port = map[string]uint16{"web": 3000}
		_, err := cmd.planSites(noResolve(), vhostSites(3000))
		Expect(err).To(MatchError(ContainSubstring("--port and --base-domain")))
	})

	It("points the guest's own name lookups at the VM itself", func() {
		plan, err := cmd.planSites(noResolve(), vhostSites(3000))
		Expect(err).NotTo(HaveOccurred())
		defer plan.closeListeners()

		Expect(plan.guestAliases()).To(Equal(map[string]string{
			"local.sws.local":     "127.0.0.1",
			"api-local.sws.local": "127.0.0.1",
		}))
	})

	It("has no guest aliases without a base domain, where the sites have no names", func() {
		plan, err := (&Cmd{}).planSites(noResolve(), twoSites())
		Expect(err).NotTo(HaveOccurred())
		defer plan.closeListeners()

		Expect(plan.guestAliases()).To(BeEmpty())
	})
})

var _ = Describe("report with a base domain", func() {
	It("names each site's hostname and tells the developer how to make it resolve", func() {
		cmd := &Cmd{BaseDomain: "sws.local", Ref: DefaultRefLabel}
		plan, err := cmd.planSites(noResolve(), vhostSites(3000))
		Expect(err).NotTo(HaveOccurred())
		defer plan.closeListeners()

		var out bytes.Buffer
		plan.report(&out)
		Expect(out.String()).To(ContainSubstring("http://local.sws.local"))
		Expect(out.String()).To(ContainSubstring("guest port 3000"))
		// The made-up domain does not resolve on a developer's machine until
		// they say it does, and that is the whole failure mode worth warning
		// about here.
		Expect(out.String()).To(ContainSubstring("/etc/hosts"))
		Expect(out.String()).To(ContainSubstring("127.0.0.1"))
	})

	It("says so when the ingress could not take the port the servers listen on", func() {
		port, ln := takePort()
		defer ln.Close()

		cmd := &Cmd{BaseDomain: "sws.local", Ref: DefaultRefLabel}
		plan, err := cmd.planSites(noResolve(), vhostSites(port))
		Expect(err).NotTo(HaveOccurred())
		defer plan.closeListeners()

		Expect(plan.ingressPort).NotTo(Equal(port))
		var out bytes.Buffer
		plan.report(&out)
		Expect(out.String()).To(ContainSubstring("Inside the VM"))
	})

	It("leaves the hint out for a name that already resolves to loopback", func() {
		cmd := &Cmd{BaseDomain: "localhost", Ref: DefaultRefLabel}
		cfg := &project.Config{Preview: project.Preview{
			Sites: map[string]project.PreviewSite{"web": {Port: 3000, Host: "{domain}"}},
		}}
		plan, err := cmd.planSites(context.Background(), cfg)
		Expect(err).NotTo(HaveOccurred())
		defer plan.closeListeners()

		var out bytes.Buffer
		plan.report(&out)
		Expect(out.String()).NotTo(ContainSubstring("/etc/hosts"))
	})
})

// echoGuest stands in for a server inside the VM: it answers every request with
// the port it was reached on and the Host header it saw.
func echoGuest(port uint16) (net.Listener, func(ctx context.Context, p uint16) (net.Conn, error)) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 4096)
				n, err := conn.Read(buf)
				if err != nil {
					return
				}
				host := ""
				for _, line := range bytes.Split(buf[:n], []byte("\r\n")) {
					if bytes.HasPrefix(bytes.ToLower(line), []byte("host:")) {
						host = string(bytes.TrimSpace(line[len("host:"):]))
					}
				}
				body := fmt.Sprintf("port=%d host=%s", port, host)
				fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
					len(body), body)
			}()
		}
	}()
	dial := func(ctx context.Context, p uint16) (net.Conn, error) {
		if p != port {
			return nil, fmt.Errorf("no server on guest port %d", p)
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", ln.Addr().String())
	}
	return ln, dial
}
