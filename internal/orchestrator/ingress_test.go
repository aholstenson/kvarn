package orchestrator

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/preview"
)

// Ingress is testable without a VM anywhere near it: the only thing the proxy
// needs from a preview is a net.Conn into the guest, and a dialer that connects
// to a local httptest server is indistinguishable from one that connects to a
// netstack. Routing, proxying, upgrades, the holding page and the 503 path all
// exercise end to end from here.

// dialSandbox is a PreviewSandbox whose guest is a local TCP address.
type dialSandbox struct {
	addr string

	mu     sync.Mutex
	closed bool
	dials  int
}

func (d *dialSandbox) DialGuest(ctx context.Context, _ uint16) (net.Conn, error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, fmt.Errorf("sandbox closed")
	}
	d.dials++
	d.mu.Unlock()

	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", d.addr)
}

func (d *dialSandbox) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
}

func (d *dialSandbox) dialCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

var _ = Describe("Preview ingress", func() {
	const host = "main.preview.example.com"

	var (
		ctx      context.Context
		svc      *Service
		store    preview.Store
		upstream *httptest.Server
		ingress  *httptest.Server
		sandbox  *dialSandbox
		// booted counts how many times the fake booter ran.
		booted  int
		bootErr error
		// blockBoot holds each boot until closed, for the holding-page specs.
		blockBoot chan struct{}
		bootMu    sync.Mutex
	)

	// upstreamHandler is what the "guest" serves. Specs replace it in a
	// BeforeEach before the server is used.
	var upstreamHandler http.Handler

	// get issues a request to the ingress with the preview's Host header.
	get := func(path string, headers map[string]string) *http.Response {
		GinkgoHelper()
		req, err := http.NewRequest(http.MethodGet, ingress.URL+path, nil)
		Expect(err).NotTo(HaveOccurred())
		req.Host = host
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := ingress.Client().Do(req)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { resp.Body.Close() })
		return resp
	}

	browserGet := func(path string) *http.Response {
		GinkgoHelper()
		return get(path, map[string]string{"Accept": "text/html,application/xhtml+xml"})
	}

	xhrGet := func(path string) *http.Response {
		GinkgoHelper()
		return get(path, map[string]string{"Accept": "application/json"})
	}

	bodyOf := func(resp *http.Response) string {
		GinkgoHelper()
		b, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		return string(b)
	}

	// registerRunning puts a running preview in the store and gives the manager
	// its in-memory half, skipping the boot entirely.
	registerRunning := func() *preview.Preview {
		GinkgoHelper()
		p := &preview.Preview{
			ID:      preview.ID("proj", "main"),
			Project: "proj",
			Ref:     "main",
			State:   preview.StateRunning,
			Sites:   []preview.Site{{Name: "web", Host: host, Port: 3000}},
		}
		Expect(store.Put(ctx, p)).To(Succeed())
		svc.previews.mu.Lock()
		svc.previews.live[p.ID] = &previewInstance{sandbox: sandbox}
		svc.previews.mu.Unlock()
		return p
	}

	// registerStopped puts a stopped preview in the store, so a request has to
	// start a boot.
	registerStopped := func() *preview.Preview {
		GinkgoHelper()
		p := &preview.Preview{
			ID:      preview.ID("proj", "main"),
			Project: "proj",
			Ref:     "main",
			State:   preview.StateStopped,
			Sites:   []preview.Site{{Name: "web", Host: host, Port: 3000}},
		}
		Expect(store.Put(ctx, p)).To(Succeed())
		return p
	}

	BeforeEach(func() {
		ctx = context.Background()
		booted = 0
		bootErr = nil
		blockBoot = nil
		upstreamHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("hello from the guest"))
		})

		upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamHandler.ServeHTTP(w, r)
		}))
		DeferCleanup(upstream.Close)

		sandbox = &dialSandbox{addr: strings.TrimPrefix(upstream.URL, "http://")}
		store = preview.NewMemStore()
		DeferCleanup(func() { Expect(store.Close()).To(Succeed()) })

		svc = NewServiceWithOpts(ServiceOpts{
			PreviewStore:  store,
			PreviewPolicy: PreviewPolicy{Domain: "preview.example.com"},
		})
		svc.previews.boot = func(_ context.Context, p *preview.Preview, logs *preview.LogBuffer) (*previewBoot, error) {
			bootMu.Lock()
			booted++
			block := blockBoot
			err := bootErr
			bootMu.Unlock()

			if block != nil {
				<-block
			}
			if err != nil {
				return nil, err
			}
			return &previewBoot{
				Sandbox: sandbox,
				Sites:   []preview.Site{{Name: "web", Host: host, Port: 3000}},
			}, nil
		}

		ingress = httptest.NewServer(PreviewIngressHandler(svc))
		DeferCleanup(ingress.Close)
	})

	Describe("starting a preview by being asked for it", func() {
		const autoHost = "pr-12.preview.example.com"

		// getAuto issues a browser request for a hostname no preview claims yet.
		getAuto := func(hostname string) *http.Response {
			GinkgoHelper()
			req, err := http.NewRequest(http.MethodGet, ingress.URL+"/", nil)
			Expect(err).NotTo(HaveOccurred())
			req.Host = hostname
			req.Header.Set("Accept", "text/html")
			resp, err := ingress.Client().Do(req)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { resp.Body.Close() })
			return resp
		}

		// resolvesTo wires auto-start to answer with a fixed outcome, standing
		// in for the route table and the forge lookup behind it.
		resolvesTo := func(target previewTarget, err error) {
			svc.previews.auto = newAutoStarter(
				func(context.Context, string) (previewTarget, error) { return target, err },
				time.Now)
		}

		It("registers and boots the preview a claimed hostname names", func() {
			resolvesTo(previewTarget{Project: "proj", Ref: "feature/login", PR: "12"}, nil)

			resp := getAuto(autoHost)
			Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
			Expect(bodyOf(resp)).To(ContainSubstring("Preparing environment"))

			p, err := store.Get(ctx, preview.ID("proj", "feature/login"))
			Expect(err).NotTo(HaveOccurred())
			Expect(p.PR).To(Equal("12"))
			Expect(p.AutoStartHost).To(Equal(autoHost))
			Eventually(func() int {
				bootMu.Lock()
				defer bootMu.Unlock()
				return booted
			}).Should(Equal(1))
		})

		It("tells a client that cannot read HTML to come back", func() {
			resolvesTo(previewTarget{Project: "proj", Ref: "feature/login", PR: "12"}, nil)

			req, err := http.NewRequest(http.MethodGet, ingress.URL+"/", nil)
			Expect(err).NotTo(HaveOccurred())
			req.Host = autoHost
			req.Header.Set("Accept", "application/json")
			resp, err := ingress.Client().Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()

			Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
			Expect(resp.Header.Get("Retry-After")).NotTo(BeEmpty())
		})

		It("says why a claimed hostname is not going to start anything", func() {
			// "No preview here" in front of a hostname the forge just handed
			// somebody is baffling; the reason is the whole answer.
			resolvesTo(previewTarget{}, errors.New("pull request 12 is closed"))

			resp := getAuto(autoHost)
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
			Expect(bodyOf(resp)).To(ContainSubstring("pull request 12 is closed"))
		})

		It("answers a hostname no project claims with the ordinary 404", func() {
			resolvesTo(previewTarget{}, preview.ErrNoRoute)

			resp := getAuto("www.preview.example.com")
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
			Expect(bodyOf(resp)).To(ContainSubstring("No preview here"))
			bootMu.Lock()
			defer bootMu.Unlock()
			Expect(booted).To(Equal(0))
		})

		It("holds off rather than refusing when it cannot answer right now", func() {
			resolvesTo(previewTarget{}, ErrAutoStartUnavailable)

			resp := getAuto(autoHost)
			Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
			Expect(resp.Header.Get("Retry-After")).NotTo(BeEmpty())
		})
	})

	Describe("routing", func() {
		It("proxies a request into the guest", func() {
			registerRunning()

			resp := browserGet("/")
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(bodyOf(resp)).To(Equal("hello from the guest"))
			Expect(sandbox.dialCount()).To(BeNumerically(">=", 1))
		})

		It("matches the host exactly, with no fallback", func() {
			registerRunning()

			req, err := http.NewRequest(http.MethodGet, ingress.URL+"/", nil)
			Expect(err).NotTo(HaveOccurred())
			req.Host = "other.preview.example.com"
			req.Header.Set("Accept", "text/html")
			resp, err := ingress.Client().Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()

			Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
			Expect(sandbox.dialCount()).To(Equal(0))
		})

		It("ignores the port in the Host header", func() {
			registerRunning()

			req, err := http.NewRequest(http.MethodGet, ingress.URL+"/", nil)
			Expect(err).NotTo(HaveOccurred())
			req.Host = host + ":8080"
			resp, err := ingress.Client().Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
		})

		It("routes each app to its own port", func() {
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("assets"))
			}))
			DeferCleanup(second.Close)

			// The sandbox picks its target from the port it is handed, the way
			// a real netstack dial does.
			ports := map[uint16]string{
				3000: strings.TrimPrefix(upstream.URL, "http://"),
				8080: strings.TrimPrefix(second.URL, "http://"),
			}
			multi := &portSandbox{ports: ports}

			p := &preview.Preview{
				ID: preview.ID("proj", "main"), Project: "proj", Ref: "main",
				State: preview.StateRunning,
				Sites: []preview.Site{
					{Name: "web", Host: host, Port: 3000},
					{Name: "assets", Host: "assets-main.preview.example.com", Port: 8080},
				},
			}
			Expect(store.Put(ctx, p)).To(Succeed())
			svc.previews.mu.Lock()
			svc.previews.live[p.ID] = &previewInstance{sandbox: multi}
			svc.previews.mu.Unlock()

			for name, want := range map[string]string{
				host:                              "hello from the guest",
				"assets-main.preview.example.com": "assets",
			} {
				req, err := http.NewRequest(http.MethodGet, ingress.URL+"/", nil)
				Expect(err).NotTo(HaveOccurred())
				req.Host = name
				resp, err := ingress.Client().Do(req)
				Expect(err).NotTo(HaveOccurred())
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				Expect(string(body)).To(Equal(want), name)
			}
		})
	})

	Describe("proxy behaviour", func() {
		It("preserves the Host the browser asked for", func() {
			var seenHost string
			upstreamHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seenHost = r.Host
				w.Write([]byte("ok"))
			})
			registerRunning()

			browserGet("/")
			Expect(seenHost).To(Equal(host))
		})

		It("sets the forwarding headers", func() {
			var seen http.Header
			upstreamHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.Header.Clone()
				w.Write([]byte("ok"))
			})
			registerRunning()

			browserGet("/")
			Expect(seen.Get("X-Forwarded-For")).NotTo(BeEmpty())
			Expect(seen.Get("X-Forwarded-Host")).To(Equal(host))
			Expect(seen.Get("X-Forwarded-Proto")).To(Equal("http"))
		})

		It("passes through an X-Forwarded-Proto set by the TLS terminator", func() {
			var seen string
			upstreamHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.Header.Get("X-Forwarded-Proto")
				w.Write([]byte("ok"))
			})
			registerRunning()

			get("/", map[string]string{"X-Forwarded-Proto": "https"})
			Expect(seen).To(Equal("https"))
		})

		It("marks every response noindex", func() {
			registerRunning()
			resp := browserGet("/")
			Expect(resp.Header.Get("X-Robots-Tag")).To(ContainSubstring("noindex"))
		})

		It("forwards the request path, query and method", func() {
			var seen string
			upstreamHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.Method + " " + r.URL.RequestURI()
				w.Write([]byte("ok"))
			})
			registerRunning()

			req, err := http.NewRequest(http.MethodPost, ingress.URL+"/api/thing?a=1&b=2", strings.NewReader("body"))
			Expect(err).NotTo(HaveOccurred())
			req.Host = host
			resp, err := ingress.Client().Do(req)
			Expect(err).NotTo(HaveOccurred())
			resp.Body.Close()

			Expect(seen).To(Equal("POST /api/thing?a=1&b=2"))
		})

		It("streams a server-sent event stream rather than buffering it", func() {
			release := make(chan struct{})
			upstreamHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				flusher, ok := w.(http.Flusher)
				Expect(ok).To(BeTrue())
				fmt.Fprint(w, "data: first\n\n")
				flusher.Flush()
				<-release
				fmt.Fprint(w, "data: second\n\n")
				flusher.Flush()
			})
			registerRunning()

			req, err := http.NewRequest(http.MethodGet, ingress.URL+"/events", nil)
			Expect(err).NotTo(HaveOccurred())
			req.Host = host
			resp, err := ingress.Client().Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()

			reader := bufio.NewReader(resp.Body)
			// The first event arrives before the handler has returned, which is
			// only true if nothing buffered the whole response.
			line, err := reader.ReadString('\n')
			Expect(err).NotTo(HaveOccurred())
			Expect(line).To(Equal("data: first\n"))

			close(release)
			_, _ = reader.ReadString('\n')
			line, err = reader.ReadString('\n')
			Expect(err).NotTo(HaveOccurred())
			Expect(line).To(Equal("data: second\n"))
		})

		It("proxies a connection upgrade", func() {
			// A minimal echo upgrade: enough to prove the hijacked bytes flow
			// both ways, without pulling in a WebSocket library.
			upstreamHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				Expect(r.Header.Get("Upgrade")).To(Equal("echo"))
				conn, _, err := w.(http.Hijacker).Hijack()
				Expect(err).NotTo(HaveOccurred())
				defer conn.Close()
				fmt.Fprint(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: echo\r\nConnection: Upgrade\r\n\r\n")
				io.Copy(conn, conn)
			})
			registerRunning()

			conn, err := net.Dial("tcp", strings.TrimPrefix(ingress.URL, "http://"))
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(10 * time.Second))

			fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: echo\r\nConnection: Upgrade\r\n\r\n", host)

			reader := bufio.NewReader(conn)
			statusLine, err := reader.ReadString('\n')
			Expect(err).NotTo(HaveOccurred())
			Expect(statusLine).To(ContainSubstring("101"))
			// Drain the remaining response headers.
			for {
				line, err := reader.ReadString('\n')
				Expect(err).NotTo(HaveOccurred())
				if line == "\r\n" {
					break
				}
			}

			fmt.Fprint(conn, "ping\n")
			echoed, err := reader.ReadString('\n')
			Expect(err).NotTo(HaveOccurred())
			Expect(echoed).To(Equal("ping\n"))
		})

		It("answers 502 when the guest refuses the connection", func() {
			registerRunning()
			upstream.Close()

			resp := xhrGet("/")
			Expect(resp.StatusCode).To(Equal(http.StatusBadGateway))
		})
	})

	Describe("a preview that is not running", func() {
		It("returns the holding page to a browser and starts a boot", func() {
			blockBoot = make(chan struct{})
			DeferCleanup(func() { close(blockBoot) })
			registerStopped()

			resp := browserGet("/")
			Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
			Expect(resp.Header.Get("Content-Type")).To(ContainSubstring("text/html"))
			Expect(resp.Header.Get("Retry-After")).To(Equal("15"))

			body := bodyOf(resp)
			Expect(body).To(ContainSubstring("Preparing environment"))
			Expect(body).To(ContainSubstring(previewStatusPath))
			// The page has to know both halves of the readiness signal: the
			// header it watches for, and what it says while the reload runs.
			Expect(body).To(ContainSubstring(previewStatusHeader))
			Expect(body).To(ContainSubstring("Redirecting to preview"))

			Eventually(func() int {
				bootMu.Lock()
				defer bootMu.Unlock()
				return booted
			}).Should(Equal(1))
		})

		It("returns 503 rather than HTML to a request that did not ask for a page", func() {
			blockBoot = make(chan struct{})
			DeferCleanup(func() { close(blockBoot) })
			registerStopped()

			resp := xhrGet("/api/thing")
			Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
			Expect(resp.Header.Get("Content-Type")).NotTo(ContainSubstring("text/html"))
			Expect(resp.Header.Get("Retry-After")).To(Equal("15"))
		})

		It("returns 503 rather than HTML to a POST, even from a browser", func() {
			blockBoot = make(chan struct{})
			DeferCleanup(func() { close(blockBoot) })
			registerStopped()

			req, err := http.NewRequest(http.MethodPost, ingress.URL+"/submit", strings.NewReader("x=1"))
			Expect(err).NotTo(HaveOccurred())
			req.Host = host
			req.Header.Set("Accept", "text/html")
			resp, err := ingress.Client().Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()

			Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
			Expect(resp.Header.Get("Content-Type")).NotTo(ContainSubstring("text/html"))
		})

		It("serves the app once the boot finishes", func() {
			registerStopped()

			resp := browserGet("/")
			Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))

			Eventually(func() int {
				resp := browserGet("/")
				return resp.StatusCode
			}).Should(Equal(http.StatusOK))
		})

		It("boots once for a burst of requests", func() {
			blockBoot = make(chan struct{})
			registerStopped()

			var wg sync.WaitGroup
			for range 8 {
				wg.Add(1)
				go func() {
					defer GinkgoRecover()
					defer wg.Done()
					browserGet("/")
				}()
			}
			wg.Wait()

			bootMu.Lock()
			count := booted
			bootMu.Unlock()
			Expect(count).To(Equal(1))
			close(blockBoot)
		})
	})

	Describe("the status endpoint", func() {
		It("reports the live boot phase", func() {
			blockBoot = make(chan struct{})
			DeferCleanup(func() { close(blockBoot) })
			registerStopped()

			browserGet("/")
			Eventually(func() string {
				resp := xhrGet(previewStatusPath)
				var status previewStatus
				Expect(json.NewDecoder(resp.Body).Decode(&status)).To(Succeed())
				return status.State
			}).Should(Equal(string(preview.StateBooting)))
		})

		It("reports a failure with its reason", func() {
			bootErr = fmt.Errorf("setup step \"build\" failed")
			registerStopped()

			browserGet("/")
			Eventually(func() string {
				resp := xhrGet(previewStatusPath)
				var status previewStatus
				Expect(json.NewDecoder(resp.Body).Decode(&status)).To(Succeed())
				return status.Error
			}).Should(ContainSubstring("build"))
		})

		It("belongs to the app once the preview is serving", func() {
			var seenPath string
			upstreamHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seenPath = r.URL.Path
				w.Write([]byte("app handled it"))
			})
			registerRunning()

			resp := xhrGet(previewStatusPath)
			Expect(bodyOf(resp)).To(Equal("app handled it"))
			Expect(seenPath).To(Equal(previewStatusPath))
		})

		It("marks its own answers, so the page can tell them from the app's", func() {
			blockBoot = make(chan struct{})
			DeferCleanup(func() { close(blockBoot) })
			registerStopped()

			browserGet("/")
			Eventually(func() string {
				return xhrGet(previewStatusPath).Header.Get(previewStatusHeader)
			}).Should(Equal("1"))
		})

		It("drops the mark once the preview serves, which is what tells the page to reload", func() {
			// Most apps have no route here and answer 404. The page must read
			// that as "the preview is up" rather than as a failed poll, so the
			// mark's absence is the signal and the body is never parsed.
			upstreamHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.NotFound(w, r)
			})
			registerRunning()

			resp := xhrGet(previewStatusPath)
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
			Expect(resp.Header.Get(previewStatusHeader)).To(BeEmpty())
		})
	})

	Describe("idle tracking", func() {
		It("stamps the last-request time on every request", func() {
			p := registerRunning()
			before, err := store.Get(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(before.LastRequestAt.IsZero()).To(BeTrue())

			browserGet("/")

			after, err := store.Get(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(after.LastRequestAt.IsZero()).To(BeFalse())
		})
	})

	Describe("when previews are not configured", func() {
		It("answers 404 to everything", func() {
			bare := NewServiceWithOpts(ServiceOpts{})
			server := httptest.NewServer(PreviewIngressHandler(bare))
			DeferCleanup(server.Close)

			req, err := http.NewRequest(http.MethodGet, server.URL+"/", nil)
			Expect(err).NotTo(HaveOccurred())
			req.Host = host
			resp, err := server.Client().Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		})
	})
})

// portSandbox dials a different local address per guest port, so one spec can
// prove that each app's traffic reaches its own server.
type portSandbox struct {
	ports map[uint16]string
}

func (p *portSandbox) DialGuest(ctx context.Context, port uint16) (net.Conn, error) {
	addr, ok := p.ports[port]
	if !ok {
		return nil, fmt.Errorf("nothing listening on port %d", port)
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", addr)
}

func (p *portSandbox) Close() {}
