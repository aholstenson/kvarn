package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/aholstenson/kvarn/internal/preview"
	"github.com/aholstenson/kvarn/internal/session"
)

// Preview ingress is a plain-HTTP listener that routes by Host into a VM. It is
// deliberately not mounted on PublicMux: that mux is authenticated ConnectRPC,
// and this is unauthenticated browser traffic going somewhere else entirely.
//
// TLS terminates outside kvarn. The listener is meant to be bound to an address
// only the fronting layer can reach — a tailnet IP, a loopback address behind
// Caddy — and that layer is also where access control lives. This matters more
// than it usually would: a preview runs unreviewed branch code that an attacker
// can drive, with the project's real resolved secrets behind its egress proxy.
// See docs/how-to/preview-environments.md.

// previewStatusPath is the endpoint the holding page polls for boot progress.
// It sits under a /_kvarn/ prefix so it cannot collide with an app's own
// routes, and it is only answered while the preview is not yet serving —
// once it is, the path belongs to the app like everything else.
const previewStatusPath = "/_kvarn/preview/status"

// previewStatusHeader marks a reply as the ingress's own. The holding page
// needs to tell "the preview is still booting" from "the preview is serving and
// the app is answering this path now", and the body cannot say so: once the
// path belongs to the app, the reply is whatever that app makes of an unknown
// route, which is usually a 404 in some shape the page cannot read.
//
// The absence of this header is therefore the readiness signal, and it is a
// header rather than a status code because an app is free to answer the path
// with a 200. Every reply the ingress writes itself carries it — the holding
// page, the 404s, the lookup failure — because the alternative reading is that
// a single transient error means the preview came up, which navigates the
// person waiting off the holding page and into that error.
const previewStatusHeader = "X-Kvarn-Preview-Status"

// previewRetryAfter is what a non-browser client is told to wait before trying
// again. A first boot takes longer than this, but the point is a cadence a
// polling client can follow, not an accurate prediction.
const previewRetryAfter = 15

// previewDialTimeout bounds how long a single connection into the guest may
// take to establish. The VM is on a userspace network in this process, so a
// dial that is slow is a dial that is never going to connect.
const previewDialTimeout = 10 * time.Second

// previewIdleConnTimeout is how long an unused connection into a guest is kept
// for the next request. A browser fetching a page's assets comes back within
// milliseconds, so this only has to outlive a page load; keeping it much longer
// would hold bridge streams open for a preview nobody is looking at any more.
const previewIdleConnTimeout = 30 * time.Second

// previewMaxIdleConnsPerSite is how many spare connections one site may keep.
// Browsers open around six per hostname and asset-heavy pages use all of them,
// so a smaller pool would send most requests back through a fresh guest dial.
const previewMaxIdleConnsPerSite = 8

// PreviewIngressHandler builds the HTTP handler that serves preview traffic.
func PreviewIngressHandler(svc *Service) http.Handler {
	h := &previewIngress{svc: svc, log: slog.With("component", "preview-ingress")}
	h.guestProxy = h.newProxy()
	svc.previews.onInstanceGoneCallback(h.closeIdleGuestConns)
	return h
}

type previewIngress struct {
	svc *Service
	log *slog.Logger
	// guestProxy carries every preview's traffic. It is built once and shared:
	// its transport is where connections into the guests are pooled, and a
	// per-request proxy would dial the bridge again for every asset on a page.
	guestProxy *httputil.ReverseProxy
}

// proxyTarget is which preview and which of its ports a request is bound for.
// It rides on the request context because the reverse proxy is shared, so the
// destination cannot be captured in its closures.
type proxyTarget struct {
	preview *preview.Preview
	site    preview.Site
}

type proxyTargetKey struct{}

// targetFrom returns the preview and site a proxied request is for. The proxy
// only ever runs on requests ServeHTTP has stamped, so a missing target is a
// programming error rather than something to serve a page about.
func targetFrom(ctx context.Context) (proxyTarget, bool) {
	t, ok := ctx.Value(proxyTargetKey{}).(proxyTarget)
	return t, ok
}

func (h *previewIngress) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mgr := h.svc.previews
	if !mgr.enabled() {
		w.Header().Set(previewStatusHeader, "1")
		http.Error(w, "preview environments are not configured on this host", http.StatusNotFound)
		return
	}

	host := preview.NormalizeHost(r.Host)
	p, err := mgr.FindByHost(r.Context(), host)
	if errors.Is(err, preview.ErrNotFound) {
		// Exact match, no fallback. Serving an unknown name from whichever
		// preview happens to be running would produce something that looks like
		// it works and is showing the wrong branch.
		//
		// A name no preview claims may still be one a project has said it will
		// answer to, which is where a preview that nobody has started yet comes
		// from.
		p = h.autoStart(w, r, host)
		if p == nil {
			// Nothing to route to, and autoStart has already answered.
			return
		}
		err = nil
	}
	if err != nil {
		h.log.Error("could not resolve preview host", "host", host, "error", err)
		w.Header().Set(previewStatusHeader, "1")
		http.Error(w, "preview lookup failed", http.StatusInternalServerError)
		return
	}

	// Stamp before anything else: idle reaping measures this, and a request
	// that is about to sit on a slow upstream is still a request.
	mgr.Touch(r.Context(), p.ID)

	if r.URL.Path == previewStatusPath && !mgr.IsLive(p.ID) {
		h.writeStatus(w, r, p)
		return
	}

	if !mgr.IsLive(p.ID) {
		h.startAndHold(w, r, p)
		return
	}

	// Which site serves this name is only knowable once the preview has booted:
	// a preview that has never run has no sites recorded yet, because the host
	// patterns live in the kvarn.yml the boot clones.
	site, ok := p.SiteForHost(host)
	if !ok {
		h.notFound(w, r, host)
		return
	}

	h.proxy(w, r, p, site)
}

// autoStart tries to bring a preview into being for a hostname nothing claims
// yet. It returns the registered preview, or nil after answering the request
// itself.
//
// The three outcomes are three different answers. A name no project claims is
// the ordinary 404. A name a project claims but whose pull request is closed,
// or from a fork, is also a 404 — nothing is going to make it work — but it says
// which, because "no preview here" in front of a hostname somebody was just
// given by their forge is baffling. Anything else is temporary and gets the
// holding page's 503, so a client that retries is not told the preview will
// never exist.
func (h *previewIngress) autoStart(w http.ResponseWriter, r *http.Request, host string) *preview.Preview {
	p, err := h.svc.previews.AutoStart(r.Context(), host)
	switch {
	case err == nil:
		return p
	case errors.Is(err, preview.ErrNoRoute), errors.Is(err, ErrPreviewsDisabled):
		h.notFound(w, r, host)
	case errors.Is(err, ErrAutoStartUnavailable), errors.Is(err, ErrPreviewDraining):
		h.unavailable(w, r, "Not starting environments right now",
			"This host is not starting new preview environments at the moment. Try again shortly.")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The client went away mid-resolution; there is nobody to answer.
	default:
		h.log.Info("could not auto-start a preview", "host", host, "error", err)
		h.refused(w, r, host, err)
	}
	return nil
}

// refused answers a hostname that is claimed but cannot be started.
//
// Only a reason the resolver marked as safe to show is shown. The rest name the
// project, the repository or whatever the forge said about the credentials, and
// this reply goes to anybody who can reach the listener; the detail is in the
// log the caller already wrote.
func (h *previewIngress) refused(w http.ResponseWriter, r *http.Request, host string, cause error) {
	detail := "it could not be started"
	if reason, ok := refusalReason(cause); ok {
		detail = reason
	}

	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set(previewStatusHeader, "1")
	if !wantsHTML(r) {
		http.Error(w, fmt.Sprintf("no preview environment for %s: %s", host, detail), http.StatusNotFound)
		return
	}
	writeHTML(w, http.StatusNotFound, previewErrorPage(
		"No preview here",
		fmt.Sprintf("%s should have started a preview, but %s.", host, detail),
		"Start one with kvarn preview up <project> <ref>."))
}

// bootFailed answers a request for a preview whose last boot did not come up.
//
// Why it failed is not repeated here for the same reason refused withholds it:
// a boot error carries clone URLs, project names and setup output. It is on the
// preview's record and in `kvarn preview logs`, where somebody with an API key
// can read it.
func (h *previewIngress) bootFailed(w http.ResponseWriter, r *http.Request, p *preview.Preview) {
	h.log.Warn("serving a preview whose boot failed", "preview", p.ID, "error", p.Error)

	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(previewStatusHeader, "1")
	if !wantsHTML(r) {
		http.Error(w, "the preview environment failed to start", http.StatusServiceUnavailable)
		return
	}
	writeHTML(w, http.StatusServiceUnavailable, previewErrorPage(
		"The preview did not start",
		fmt.Sprintf("The last attempt to start a preview of %s failed.", p.Ref),
		"Run kvarn preview logs to see why."))
}

// notFound answers a hostname no preview claims.
func (h *previewIngress) notFound(w http.ResponseWriter, r *http.Request, host string) {
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set(previewStatusHeader, "1")
	if !wantsHTML(r) {
		http.Error(w, fmt.Sprintf("no preview environment is registered for %s", host), http.StatusNotFound)
		return
	}
	writeHTML(w, http.StatusNotFound, previewErrorPage(
		"No preview here",
		fmt.Sprintf("Nothing is registered for %s.", host),
		"Start one with kvarn preview up <project> <ref>."))
}

// startAndHold kicks off a boot and tells the client to come back.
//
// The two answers are not interchangeable. A browser navigating to the preview
// gets a page that explains itself and polls; anything else — an XHR, a curl, a
// health check — gets a 503 with Retry-After, because handing HTML to a request
// that asked for JSON produces a baffling error inside the app rather than a
// clear one in front of it.
func (h *previewIngress) startAndHold(w http.ResponseWriter, r *http.Request, p *preview.Preview) {
	updated, err := h.svc.previews.Ensure(r.Context(), p.ID)
	if err != nil {
		if errors.Is(err, ErrPreviewDraining) {
			h.unavailable(w, r, "This host is being taken out of service",
				"The preview cannot start here. Try again shortly.")
			return
		}
		h.log.Warn("could not start preview", "preview", p.ID, "error", err)
	}
	if updated != nil && updated.State == preview.StateFailed {
		// Ensure declined to repeat a boot that just failed. Saying so is the
		// honest answer; the holding page would poll a preview that is not
		// coming up until somebody closed the tab.
		h.bootFailed(w, r, updated)
		return
	}

	h.unavailable(w, r, "Preparing environment",
		// No duration is promised: a first boot clones the branch, installs the
		// project's dependencies and runs its setup steps, which is minutes for
		// one project and much longer for another. The phase below is what tells
		// somebody it is still moving.
		fmt.Sprintf("Starting a preview of %s. A first boot has to clone the branch and install its dependencies, so it can take a while.", p.Ref))
}

// unavailable writes the holding page or a 503, depending on what the client
// can make sense of.
func (h *previewIngress) unavailable(w http.ResponseWriter, r *http.Request, title, detail string) {
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("Retry-After", fmt.Sprintf("%d", previewRetryAfter))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(previewStatusHeader, "1")

	if !wantsHTML(r) {
		http.Error(w, title+": "+detail, http.StatusServiceUnavailable)
		return
	}
	writeHTML(w, http.StatusServiceUnavailable, previewHoldingPage(title, detail))
}

// previewStatus is what the holding page polls for.
type previewStatus struct {
	State string `json:"state"`
	// Phase is the boot's current step in words — "Cloning repository",
	// "Installing dependencies" — taken from the session the boot narrates
	// itself through. A minute-long first boot is fine; it just has to be
	// legible while it happens.
	Phase string `json:"phase"`
	// Error says that the boot failed, not why. The reason names clone URLs,
	// project names and setup output, and this endpoint answers anybody who can
	// reach the listener; `kvarn preview logs` is where the reason is read.
	Error string `json:"error,omitempty"`
	Ready bool   `json:"ready"`
}

// writeStatus answers the holding page's poll with the live boot phase.
func (h *previewIngress) writeStatus(w http.ResponseWriter, r *http.Request, p *preview.Preview) {
	status := previewStatus{
		State: string(p.State),
		Ready: h.svc.previews.IsLive(p.ID),
	}
	if p.State == preview.StateFailed {
		h.log.Warn("reporting a failed preview boot", "preview", p.ID, "error", p.Error)
		status.Error = "check kvarn preview logs for what went wrong"
	}
	status.Phase = h.bootPhase(r.Context(), p)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set(previewStatusHeader, "1")
	_ = json.NewEncoder(w).Encode(status)
}

// bootPhase reads the human-readable phase off the boot's session, falling back
// to the preview's own state when there is no session to read.
func (h *previewIngress) bootPhase(ctx context.Context, p *preview.Preview) string {
	// A preview writing its state out still carries the session of the boot that
	// brought it up, and that session's last word was "ready". Reading it here
	// would tell somebody watching the holding page that the preview is running.
	if p.State == preview.StateStopping || p.SessionID == "" || h.svc.sessionMgr == nil {
		return previewPhaseFallback(p.State)
	}
	sess, err := h.svc.sessionMgr.Get(ctx, p.SessionID)
	if err != nil || sess == nil {
		return previewPhaseFallback(p.State)
	}
	if sess.Message != "" {
		return sess.Message
	}
	return previewPhaseLabel(sess.State)
}

// previewPhaseFallback describes a preview with no boot session to report from.
func previewPhaseFallback(state preview.State) string {
	switch state {
	case preview.StateBooting:
		return "Starting"
	case preview.StateFailed:
		return "Failed"
	case preview.StateRunning:
		return "Running"
	case preview.StateStopping:
		return "Saving state"
	default:
		return "Stopped"
	}
}

// previewPhaseLabel turns a session state into the words the holding page shows.
func previewPhaseLabel(state session.State) string {
	switch state {
	case session.StateCloning:
		return "Cloning repository"
	case session.StateProvisioning:
		return "Provisioning VM"
	case session.StateTransferring:
		return "Transferring files"
	case session.StateInstallingDependencies:
		return "Installing dependencies"
	case session.StateSetup:
		return "Running setup"
	case session.StateRunning:
		return "Starting services"
	case session.StateFailed:
		return "Failed"
	case session.StateCompleted:
		return "Ready"
	default:
		return "Starting"
	}
}

// newProxy builds the shared reverse proxy. Everything that varies per request
// comes off the request's target rather than out of a closure, so one proxy —
// and with it one pool of connections into the guests — serves every preview.
func (h *previewIngress) newProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			t, ok := targetFrom(pr.In.Context())
			if !ok {
				return
			}
			pr.Out.URL.Scheme = "http"
			// The URL's host is what the transport pools connections under, so
			// it has to name one site of one preview and nothing else. The
			// hostname the request came in on does exactly that: it is what
			// resolved to this preview in the first place.
			pr.Out.URL.Host = previewPoolKey(pr.In.Host, t.site.Port)
			// The app sees the hostname the browser asked for, not the
			// synthetic one the transport dials: absolute URLs it generates
			// have to match what is in the address bar.
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			// TLS terminated in front of us, so the app cannot see the scheme
			// from the connection and has to be told.
			pr.Out.Header.Set("X-Forwarded-Proto", forwardedProto(pr.In))
		},
		Transport: &http.Transport{
			// Every connection goes through the preview's own netstack; the
			// address in the URL exists only to key the connection pool.
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				t, ok := targetFrom(ctx)
				if !ok {
					return nil, errors.New("no preview target on this request")
				}
				dialCtx, cancel := context.WithTimeout(ctx, previewDialTimeout)
				defer cancel()
				conn, live, err := h.svc.previews.DialGuest(dialCtx, t.preview.ID, t.site.Port)
				if err != nil {
					return nil, err
				}
				if !live {
					return nil, fmt.Errorf("preview %s is not running", t.preview.ID)
				}
				return conn, nil
			},
			// A preview runs one small server behind a userspace network, so
			// the defaults sized for a public-internet client pool are wrong
			// here in both directions. Reaching a guest costs a round trip
			// across the bridge, which is why a page's worth of assets has to
			// be able to share the connections it opens.
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   previewMaxIdleConnsPerSite,
			IdleConnTimeout:       previewIdleConnTimeout,
			ResponseHeaderTimeout: 5 * time.Minute,
			// Compression is negotiated between the browser and the app; we
			// have no reason to insert ourselves into it.
			DisableCompression: true,
		},
		// WebSocket and SSE both depend on the response streaming through
		// rather than being buffered into a whole message first.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			// A preview must never end up in a search index: the hostnames are
			// guessable from branch names and the content is unreviewed.
			resp.Header.Set("X-Robots-Tag", "noindex, nofollow")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// A failure is the one place the target may be missing, so the
			// labels fall back rather than assuming it is there.
			previewID, siteName := "unknown", "requested"
			if t, ok := targetFrom(r.Context()); ok {
				previewID, siteName = t.preview.ID, t.site.Name
			}
			h.log.Warn("preview upstream failed",
				"preview", previewID, "site", siteName, "error", err)
			w.Header().Set("X-Robots-Tag", "noindex")
			if !wantsHTML(r) {
				http.Error(w, "preview upstream unavailable", http.StatusBadGateway)
				return
			}
			writeHTML(w, http.StatusBadGateway, previewErrorPage(
				"The app is not answering",
				fmt.Sprintf("The %s service in this preview did not respond.", siteName),
				"Check kvarn preview logs for what it printed."))
		},
		ErrorLog: slog.NewLogLogger(h.log.Handler(), slog.LevelDebug),
	}
}

// previewPoolKey names one site of one preview for the transport's connection
// pool. Requests that share it share connections, so it is built from the
// hostname that routes to the preview rather than from anything a client sends.
func previewPoolKey(host string, port uint16) string {
	return fmt.Sprintf("%s:%d", preview.NormalizeHost(host), port)
}

// proxy forwards the request into the guest.
func (h *previewIngress) proxy(w http.ResponseWriter, r *http.Request, p *preview.Preview, site preview.Site) {
	ctx := context.WithValue(r.Context(), proxyTargetKey{}, proxyTarget{preview: p, site: site})
	h.guestProxy.ServeHTTP(w, r.WithContext(ctx))
}

// closeIdleGuestConns drops the connections the proxy is holding open for
// previews that are no longer running. A pooled connection outlives the request
// that opened it, so without this a stopped preview's bridge streams would stay
// up until the pool expired them.
func (h *previewIngress) closeIdleGuestConns() {
	if tr, ok := h.guestProxy.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

// forwardedProto is the scheme the client used, which is https whenever
// something in front terminated TLS and said so.
func forwardedProto(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return proto
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// wantsHTML reports whether this request came from something that would render
// a page. Only a GET or HEAD that says it accepts HTML qualifies: everything
// else is better served a status code it can act on.
func wantsHTML(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	accept := r.Header.Get("Accept")
	if accept == "" {
		return false
	}
	for _, part := range strings.Split(accept, ",") {
		media := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if media == "text/html" {
			return true
		}
	}
	return false
}

func writeHTML(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// previewHoldingPage is what a browser sees while a preview boots. It polls the
// status endpoint and reloads when the preview starts serving, so the person
// waiting sees the phases go by instead of a spinner and does not have to guess
// when to hit refresh.
func previewHoldingPage(title, detail string) string {
	return fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>%[1]s</title>
<style>
  :root { color-scheme: light dark; }
  body { margin: 0; min-height: 100vh; display: grid; place-items: center;
         font: 16px/1.5 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
         background: Canvas; color: CanvasText; }
  main { max-width: 32rem; padding: 2rem; }
  h1 { font-size: 1.25rem; margin: 0 0 0.5rem; }
  p { margin: 0 0 1rem; opacity: 0.8; }
  .phase { display: flex; align-items: center; gap: 0.6rem; font-variant-numeric: tabular-nums; }
  .dot { width: 0.6rem; height: 0.6rem; border-radius: 50%%; background: currentColor;
         animation: pulse 1.4s ease-in-out infinite; }
  .failed { color: #c0392b; }
  @keyframes pulse { 0%%, 100%% { opacity: 0.25; } 50%% { opacity: 1; } }
  @media (prefers-reduced-motion: reduce) { .dot { animation: none; opacity: 0.6; } }
</style>
</head>
<body>
<main>
  <h1>%[1]s</h1>
  <p>%[2]s</p>
  <div class="phase" id="phase"><span class="dot"></span><span id="phase-text">Starting</span></div>
</main>
<script>
(function () {
  var text = document.getElementById("phase-text");
  var row = document.getElementById("phase");
  var going = false;
  // The reload is announced before it starts: the app's first reply has to come
  // back through the guest, which takes a moment, and the browser keeps showing
  // this page until it does. Without the message that pause reads as a stall.
  function go() {
    if (going) { return; }
    going = true;
    row.className = "phase";
    text.textContent = "Redirecting to preview";
    setTimeout(function () { location.reload(); }, 150);
  }
  function poll() {
    fetch(%[3]q, { headers: { "Accept": "application/json" }, cache: "no-store" })
      .then(function (r) {
        // Once the preview serves, this path belongs to the app and our marker
        // header is gone. Whatever answered — a 404, an SPA's index, anything —
        // is proof the preview is up, so the body is not worth reading. Every
        // reply we write ourselves carries the header, including the errors, so
        // one failed poll during a long boot is not read as readiness.
        if (r.headers.get(%[4]q) !== "1") { go(); return null; }
        // Our own 404 is the end of the road: nothing routes here and polling
        // will not change that.
        if (r.status === 404) {
          return r.text().then(function (t) {
            return { state: "failed", error: t.trim() || "no preview environment for this address" };
          });
        }
        // Any other answer of ours that is not the status document — a lookup
        // failure, a host being drained — is temporary. Keep waiting.
        if (r.status !== 200) { return {}; }
        return r.json();
      })
      .then(function (s) {
        if (!s) { return; }
        if (s.ready) { go(); return; }
        if (s.state === "failed") {
          row.className = "phase failed";
          text.textContent = s.error ? ("Failed: " + s.error) : "Failed to start";
          return;
        }
        if (s.phase) { text.textContent = s.phase; }
        setTimeout(poll, 2000);
      })
      .catch(function () { setTimeout(poll, 4000); });
  }
  setTimeout(poll, 1000);
})();
</script>
</body>
</html>
`, html.EscapeString(title), html.EscapeString(detail), previewStatusPath, previewStatusHeader)
}

// previewErrorPage is the static counterpart to the holding page: something has
// gone wrong and no amount of waiting will fix it.
func previewErrorPage(title, detail, hint string) string {
	return fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>%[1]s</title>
<style>
  :root { color-scheme: light dark; }
  body { margin: 0; min-height: 100vh; display: grid; place-items: center;
         font: 16px/1.5 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
         background: Canvas; color: CanvasText; }
  main { max-width: 32rem; padding: 2rem; }
  h1 { font-size: 1.25rem; margin: 0 0 0.5rem; }
  p { margin: 0 0 1rem; opacity: 0.8; }
  code { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 0.9em; }
</style>
</head>
<body>
<main>
  <h1>%[1]s</h1>
  <p>%[2]s</p>
  <p><code>%[3]s</code></p>
</main>
</body>
</html>
`, html.EscapeString(title), html.EscapeString(detail), html.EscapeString(hint))
}
