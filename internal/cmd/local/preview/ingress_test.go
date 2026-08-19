package preview

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ingress", func() {
	// get asks the ingress for a page as a browser would: connected to the
	// loopback listener, addressing the site by name.
	get := func(ln net.Listener, host string) (*http.Response, string) {
		req, err := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		return resp, string(body)
	}

	It("routes each hostname to the guest port its site is served from", func() {
		guest, dial := echoGuest(3000)
		defer guest.Close()

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		g := startIngress(ln, map[string]ingressRoute{
			"web.sws.local": {Site: "web", Port: 3000},
			"api.sws.local": {Site: "api", Port: 3000},
		}, dial, discardLogger(), nil)
		defer g.Close()

		resp, body := get(ln, "web.sws.local")
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring("port=3000"))
		// The site is told the name it was asked for, which is the only thing a
		// virtual-hosting server has to tell its sites apart with.
		Expect(body).To(ContainSubstring("host=web.sws.local"))

		_, body = get(ln, "api.sws.local")
		Expect(body).To(ContainSubstring("host=api.sws.local"))
	})

	It("matches the hostname however the request spells it", func() {
		guest, dial := echoGuest(3000)
		defer guest.Close()

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		g := startIngress(ln, map[string]ingressRoute{
			"web.sws.local": {Site: "web", Port: 3000},
		}, dial, discardLogger(), nil)
		defer g.Close()

		resp, _ := get(ln, "WEB.sws.local")
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
	})

	It("refuses a name no site claims rather than serving another site's page", func() {
		guest, dial := echoGuest(3000)
		defer guest.Close()

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		g := startIngress(ln, map[string]ingressRoute{
			"web.sws.local": {Site: "web", Port: 3000},
		}, dial, discardLogger(), nil)
		defer g.Close()

		resp, body := get(ln, "typo.sws.local")
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		Expect(body).To(ContainSubstring("web.sws.local"))
	})

	It("reports a site that is not listening once, however many requests arrive", func() {
		dial := func(context.Context, uint16) (net.Conn, error) {
			return nil, fmt.Errorf("connection refused")
		}

		reports := make(chan string, 8)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		g := startIngress(ln, map[string]ingressRoute{
			"web.sws.local": {Site: "web", Port: 3000},
		}, dial, discardLogger(), func(site string, port uint16, err error) {
			reports <- fmt.Sprintf("%s:%d", site, port)
		})
		defer g.Close()

		for range 3 {
			resp, _ := get(ln, "web.sws.local")
			Expect(resp.StatusCode).To(Equal(http.StatusBadGateway))
		}

		Eventually(reports).Should(Receive(Equal("web:3000")))
		Consistently(reports).ShouldNot(Receive())
	})

	It("says nothing about a site whose client gave up mid-request", func() {
		dialing := make(chan struct{})
		var once sync.Once
		dial := func(ctx context.Context, _ uint16) (net.Conn, error) {
			once.Do(func() { close(dialing) })
			<-ctx.Done()
			return nil, ctx.Err()
		}

		reports := make(chan string, 8)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		g := startIngress(ln, map[string]ingressRoute{
			"web.sws.local": {Site: "web", Port: 3000},
		}, dial, discardLogger(), func(site string, port uint16, err error) {
			reports <- site
		})
		defer g.Close()

		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ln.Addr().String()+"/", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Host = "web.sws.local"

		done := make(chan struct{})
		go func() {
			defer close(done)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}()

		Eventually(dialing).Should(BeClosed())
		cancel()
		Eventually(done).Should(BeClosed())

		Consistently(reports).ShouldNot(Receive())
	})
})
