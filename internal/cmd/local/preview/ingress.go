package preview

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aholstenson/kvarn/internal/preview"
)

// Domain-mode ingress is the developer-machine counterpart of the
// orchestrator's preview ingress: one listener, routing by Host header into the
// guest port that site's server is on. It matches by name exactly and never
// falls back to another site, because serving an unknown name from whichever
// site happens to be first produces something that looks like it works and is
// showing the wrong thing.
//
// It speaks HTTP rather than splicing raw TCP because Host is the routing key
// and only an HTTP parse yields it. Connection upgrades — WebSockets, and the
// event streams a dev server's hot reload runs on — are carried by the reverse
// proxy, so a preview under a fake domain behaves like one under a real one.

// ingressShutdownTimeout bounds how long in-flight requests may take to finish
// when the preview stops. Anything still running by then is a stream that will
// never end on its own, such as a hot-reload channel.
const ingressShutdownTimeout = 3 * time.Second

// ingressRoute is where one hostname's traffic goes.
type ingressRoute struct {
	Site string
	Port uint16
}

// ingress serves every site of a preview on one host port.
type ingress struct {
	routes map[string]ingressRoute
	dial   func(ctx context.Context, port uint16) (net.Conn, error)
	log    *slog.Logger
	// onUnreachable reports the first request that could not be delivered to a
	// site. Without it the only symptom is a bad gateway page, which reads as
	// kvarn being broken rather than the site's server not being up.
	onUnreachable func(site string, guestPort uint16, err error)

	srv   *http.Server
	proxy *httputil.ReverseProxy

	done chan struct{}
	once sync.Once
	// reported keeps the unreachable notice to one line per site: a browser
	// opens many connections, and repeating one diagnosis for each buries
	// everything else.
	reported sync.Map
}

// startIngress serves routes on an already-bound listener until Close.
func startIngress(
	ln net.Listener,
	routes map[string]ingressRoute,
	dial func(context.Context, uint16) (net.Conn, error),
	log *slog.Logger,
	onUnreachable func(site string, guestPort uint16, err error),
) *ingress {
	g := &ingress{
		routes:        routes,
		dial:          dial,
		log:           log,
		onUnreachable: onUnreachable,
		done:          make(chan struct{}),
	}

	// The transport dials the guest instead of the network, choosing the port
	// from the hostname the director put in the outbound URL. Routing through
	// the address this way keeps one transport — and therefore one connection
	// pool and one upgrade path — for every site.
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				host = addr
			}
			route, ok := g.routes[preview.NormalizeHost(host)]
			if !ok {
				return nil, fmt.Errorf("no preview site answers for %s", host)
			}
			return g.dial(ctx, route.Port)
		},
		// Every connection is a fresh dial into the guest, so idle sockets
		// would only hold bridge resources open between requests.
		DisableKeepAlives: true,
	}

	g.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = "http"
			// The inbound Host is both the routing key and what the site is
			// called, so it stays on the request: a virtual-hosting server has
			// nothing else to tell its sites apart with.
			r.Out.URL.Host = r.In.Host
			r.Out.Host = r.In.Host
			r.SetXForwarded()
		},
		ErrorHandler: g.handleError,
		Transport:    transport,
	}

	g.srv = &http.Server{Handler: g}
	go func() {
		if err := g.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Debug("preview ingress stopped", "error", err)
		}
	}()
	return g
}

func (g *ingress) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := preview.NormalizeHost(r.Host)
	if _, ok := g.routes[host]; !ok {
		g.notFound(w, host)
		return
	}
	g.proxy.ServeHTTP(w, r)
}

// notFound answers a name no site claims, naming the ones that exist: under a
// made-up domain the usual cause is a typo or a stale /etc/hosts line.
func (g *ingress) notFound(w http.ResponseWriter, host string) {
	names := make([]string, 0, len(g.routes))
	for name := range g.routes {
		names = append(names, name)
	}
	sort.Strings(names)
	http.Error(w, fmt.Sprintf(
		"no preview site answers for %s. This preview serves: %s",
		host, strings.Join(names, ", ")), http.StatusNotFound)
}

// handleError answers a request that could not be delivered into the guest,
// which is what a server that has not bound its port looks like from here.
func (g *ingress) handleError(w http.ResponseWriter, r *http.Request, err error) {
	host := preview.NormalizeHost(r.Host)
	route := g.routes[host]
	g.log.Debug("preview ingress could not reach site", "site", route.Site, "port", route.Port, "error", err)
	// A client that goes away mid-request — a reload, a closed tab, the preview
	// stopping — cancels its context, and that arrives here as a proxy error like
	// any other. It says nothing about whether the site is listening, so it must
	// not spend the one notice this site gets on a diagnosis that is wrong.
	if g.onUnreachable != nil && !canceled(r.Context(), err) {
		if _, seen := g.reported.LoadOrStore(host, true); !seen {
			g.onUnreachable(route.Site, route.Port, err)
		}
	}
	http.Error(w, fmt.Sprintf("preview site %s is not answering on guest port %d",
		route.Site, route.Port), http.StatusBadGateway)
}

// Close stops serving and gives in-flight requests a moment to finish.
func (g *ingress) Close() error {
	g.once.Do(func() {
		close(g.done)
		ctx, cancel := context.WithTimeout(context.Background(), ingressShutdownTimeout)
		defer cancel()
		if err := g.srv.Shutdown(ctx); err != nil {
			g.srv.Close()
		}
	})
	return nil
}
