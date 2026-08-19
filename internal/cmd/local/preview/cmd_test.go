package preview

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/project"
)

// twoSites is a preview declaring the shape most repositories have: a couple of
// named servers on fixed guest ports.
func twoSites() *project.Config {
	return &project.Config{Preview: project.Preview{
		Sites: map[string]project.PreviewSite{
			"web": {Port: 3000},
			"api": {Port: 8080},
		},
	}}
}

// takePort binds a loopback port and returns it with the listener holding it, so
// a spec can make a specific port unavailable.
func takePort() (uint16, net.Listener) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	return uint16(ln.Addr().(*net.TCPAddr).Port), ln
}

var _ = Describe("planSites", func() {
	It("gives every site a loopback URL, in name order", func() {
		cmd := &Cmd{}
		bound, err := cmd.planSites(context.Background(), twoSites())
		Expect(err).NotTo(HaveOccurred())
		defer bound.closeListeners()

		Expect(bound.sites).To(HaveLen(2))
		Expect(bound.sites[0].Name).To(Equal("api"))
		Expect(bound.sites[1].Name).To(Equal("web"))
		for _, site := range bound.sites {
			Expect(site.URL).To(HavePrefix("http://localhost:"))
		}
	})

	It("serves a site on its own port when that port is free", func() {
		port, ln := takePort()
		Expect(ln.Close()).To(Succeed())

		cfg := &project.Config{Preview: project.Preview{
			Sites: map[string]project.PreviewSite{"web": {Port: port}},
		}}
		bound, err := (&Cmd{}).planSites(context.Background(), cfg)
		Expect(err).NotTo(HaveOccurred())
		defer bound.closeListeners()

		Expect(bound.sites[0].URL).To(Equal(fmt.Sprintf("http://localhost:%d", port)))
		Expect(bound.sites[0].GuestPort).To(Equal(port))
	})

	It("falls back to a free port when the site's own port is taken", func() {
		port, ln := takePort()
		defer ln.Close()

		cfg := &project.Config{Preview: project.Preview{
			Sites: map[string]project.PreviewSite{"web": {Port: port}},
		}}
		bound, err := (&Cmd{}).planSites(context.Background(), cfg)
		Expect(err).NotTo(HaveOccurred())
		defer bound.closeListeners()

		Expect(bound.sites[0].URL).NotTo(Equal(fmt.Sprintf("http://localhost:%d", port)))
		// The forward still goes to the port the server inside the VM listens on.
		Expect(bound.sites[0].GuestPort).To(Equal(port))
	})

	It("gives sites that share a guest port a host port each", func() {
		cfg := &project.Config{Preview: project.Preview{
			Sites: map[string]project.PreviewSite{
				"web":    {Port: 80},
				"assets": {Port: 80, Host: "assets-{ref}.{domain}"},
			},
		}}
		bound, err := (&Cmd{}).planSites(context.Background(), cfg)
		Expect(err).NotTo(HaveOccurred())
		defer bound.closeListeners()

		Expect(bound.sites).To(HaveLen(2))
		Expect(bound.sites[0].URL).NotTo(Equal(bound.sites[1].URL))
		// Both forward into the one server that binds the shared guest port.
		Expect(bound.sites[0].GuestPort).To(Equal(uint16(80)))
		Expect(bound.sites[1].GuestPort).To(Equal(uint16(80)))
	})

	It("honours --port for a site", func() {
		port, ln := takePort()
		Expect(ln.Close()).To(Succeed())

		cmd := &Cmd{Port: map[string]uint16{"web": port}}
		bound, err := cmd.planSites(context.Background(), twoSites())
		Expect(err).NotTo(HaveOccurred())
		defer bound.closeListeners()

		for _, site := range bound.sites {
			if site.Name == "web" {
				Expect(site.URL).To(Equal(fmt.Sprintf("http://localhost:%d", port)))
				Expect(site.GuestPort).To(Equal(uint16(3000)))
			}
		}
	})

	It("refuses --port for a site the repository does not declare", func() {
		cmd := &Cmd{Port: map[string]uint16{"worker": 9000}}
		_, err := cmd.planSites(context.Background(), twoSites())
		Expect(err).To(MatchError(ContainSubstring(`--port names site "worker"`)))
	})

	It("refuses a --port that is already in use rather than moving the site", func() {
		port, ln := takePort()
		defer ln.Close()

		cmd := &Cmd{Port: map[string]uint16{"web": port}}
		_, err := cmd.planSites(context.Background(), twoSites())
		Expect(err).To(MatchError(ContainSubstring(fmt.Sprintf("port %d for site %q is already in use", port, "web"))))
	})
})

var _ = Describe("forwarder", func() {
	It("carries a connection through to the guest port", func() {
		// Stand in for the guest: an echo server the dial function reaches
		// instead of a VM.
		guest, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		defer guest.Close()
		go func() {
			for {
				conn, err := guest.Accept()
				if err != nil {
					return
				}
				go func() {
					defer conn.Close()
					buf := make([]byte, 64)
					n, err := conn.Read(buf)
					if err != nil {
						return
					}
					conn.Write([]byte("echo:" + string(buf[:n])))
				}()
			}
		}()

		var dialedPort uint16
		dial := func(ctx context.Context, port uint16) (net.Conn, error) {
			dialedPort = port
			var d net.Dialer
			return d.DialContext(ctx, "tcp", guest.Addr().String())
		}

		host, err := bindHostPort(0)
		Expect(err).NotTo(HaveOccurred())
		fw := startForward(host, 3000, dial, discardLogger(), nil)
		defer fw.Close()

		conn, err := net.Dial("tcp", host.Addr().String())
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close()

		_, err = conn.Write([]byte("hello"))
		Expect(err).NotTo(HaveOccurred())
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(buf[:n])).To(Equal("echo:hello"))
		Expect(dialedPort).To(Equal(uint16(3000)))
	})

	It("closes the client connection when the guest cannot be reached", func() {
		dial := func(context.Context, uint16) (net.Conn, error) {
			return nil, fmt.Errorf("nothing listening")
		}

		host, err := bindHostPort(0)
		Expect(err).NotTo(HaveOccurred())
		fw := startForward(host, 3000, dial, discardLogger(), nil)
		defer fw.Close()

		conn, err := net.Dial("tcp", host.Addr().String())
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close()

		buf := make([]byte, 1)
		_, err = conn.Read(buf)
		Expect(err).To(HaveOccurred())
	})

	It("reports an unreachable guest once, however many clients connect", func() {
		dial := func(context.Context, uint16) (net.Conn, error) {
			return nil, fmt.Errorf("connection refused")
		}

		reports := make(chan error, 8)
		host, err := bindHostPort(0)
		Expect(err).NotTo(HaveOccurred())
		fw := startForward(host, 3000, dial, discardLogger(), func(err error) { reports <- err })
		defer fw.Close()

		for range 3 {
			conn, err := net.Dial("tcp", host.Addr().String())
			Expect(err).NotTo(HaveOccurred())
			buf := make([]byte, 1)
			_, _ = conn.Read(buf)
			conn.Close()
		}

		Eventually(reports).Should(Receive(MatchError(ContainSubstring("connection refused"))))
		Consistently(reports).ShouldNot(Receive())
	})

	It("says nothing when the dial is cancelled rather than refused", func() {
		dial := func(context.Context, uint16) (net.Conn, error) {
			return nil, context.Canceled
		}

		reports := make(chan error, 8)
		host, err := bindHostPort(0)
		Expect(err).NotTo(HaveOccurred())
		fw := startForward(host, 3000, dial, discardLogger(), func(err error) { reports <- err })
		defer fw.Close()

		conn, err := net.Dial("tcp", host.Addr().String())
		Expect(err).NotTo(HaveOccurred())
		buf := make([]byte, 1)
		_, _ = conn.Read(buf)
		conn.Close()

		Consistently(reports).ShouldNot(Receive())
	})
})

var _ = Describe("report", func() {
	It("names every site, its local URL and the guest port behind it", func() {
		bound, err := (&Cmd{}).planSites(context.Background(), twoSites())
		Expect(err).NotTo(HaveOccurred())
		defer bound.closeListeners()

		var out bytes.Buffer
		bound.report(&out)
		Expect(out.String()).To(ContainSubstring("api"))
		Expect(out.String()).To(ContainSubstring("guest port 8080"))
		Expect(out.String()).To(ContainSubstring("web"))
		Expect(out.String()).To(ContainSubstring("guest port 3000"))
	})

	It("still has sites to report once forwarding has started", func() {
		bound, err := (&Cmd{}).planSites(context.Background(), twoSites())
		Expect(err).NotTo(HaveOccurred())
		defer bound.closeListeners()

		dial := func(context.Context, uint16) (net.Conn, error) {
			return nil, fmt.Errorf("nothing listening")
		}
		fw := bound.serve(dial, nil)
		defer fw.close()

		var out bytes.Buffer
		bound.report(&out)
		Expect(out.String()).To(ContainSubstring("guest port 3000"))
	})
})

var _ = Describe("serviceLog", func() {
	It("holds output until the terminal is handed over, then streams it", func() {
		log := newServiceLog()
		log.write("web", "first line\n")
		Expect(log.streaming()).To(BeFalse())

		var out bytes.Buffer
		log.streamTo(&out)
		Expect(out.String()).To(Equal("web | first line\n"))

		log.write("web", "second line\n")
		Expect(out.String()).To(HaveSuffix("web | second line\n"))
		Expect(log.streaming()).To(BeTrue())
	})

	It("prefixes each whole line and holds a partial one back", func() {
		log := newServiceLog()
		var out bytes.Buffer
		log.streamTo(&out)

		log.write("api", "half a ")
		Expect(out.String()).To(BeEmpty())
		log.write("api", "line\nnext\n")
		Expect(out.String()).To(Equal("api | half a line\napi | next\n"))
	})

	It("marks kvarn's own notes apart from what a service printed", func() {
		log := newServiceLog()
		var out bytes.Buffer
		log.streamTo(&out)
		log.note("web exited with status 1")
		Expect(out.String()).To(Equal("==> web exited with status 1\n"))
	})

	It("dumps held output, including a partial trailing line, when the boot fails", func() {
		log := newServiceLog()
		log.write("web", "starting up\ncrashed here")

		var out bytes.Buffer
		log.dump(&out)
		Expect(out.String()).To(ContainSubstring("web | starting up\n"))
		Expect(out.String()).To(ContainSubstring("web | crashed here\n"))

		// Everything held has been written; a second dump has nothing to add.
		var again bytes.Buffer
		log.dump(&again)
		Expect(again.String()).To(BeEmpty())
	})
})

// discardLogger keeps the forwarder's debug output out of the spec output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
