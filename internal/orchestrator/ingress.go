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
	"net/url"
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

// previewRetryAfter is what a non-browser client is told to wait before trying
// again. A first boot takes longer than this, but the point is a cadence a
// polling client can follow, not an accurate prediction.
const previewRetryAfter = 15

// previewDialTimeout bounds how long a single connection into the guest may
// take to establish. The VM is on a userspace network in this process, so a
// dial that is slow is a dial that is never going to connect.
const previewDialTimeout = 10 * time.Second

// PreviewIngressHandler builds the HTTP handler that serves preview traffic.
func PreviewIngressHandler(svc *Service) http.Handler {
	return &previewIngress{svc: svc, log: slog.With("component", "preview-ingress")}
}

type previewIngress struct {
	svc *Service
	log *slog.Logger
}

func (h *previewIngress) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mgr := h.svc.previews
	if !mgr.enabled() {
		http.Error(w, "preview environments are not configured on this host", http.StatusNotFound)
		return
	}

	host := preview.NormalizeHost(r.Host)
	p, err := mgr.FindByHost(r.Context(), host)
	if errors.Is(err, preview.ErrNotFound) {
		// Exact match, no fallback. Serving an unknown name from whichever
		// preview happens to be running would produce something that looks like
		// it works and is showing the wrong branch.
		h.notFound(w, r, host)
		return
	}
	if err != nil {
		h.log.Error("could not resolve preview host", "host", host, "error", err)
		http.Error(w, "preview lookup failed", http.StatusInternalServerError)
		return
	}

	app, ok := p.AppForHost(host)
	if !ok {
		h.notFound(w, r, host)
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

	h.proxy(w, r, p, app)
}

// notFound answers a hostname no preview claims.
func (h *previewIngress) notFound(w http.ResponseWriter, r *http.Request, host string) {
	w.Header().Set("X-Robots-Tag", "noindex")
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
	if _, err := h.svc.previews.Ensure(r.Context(), p.ID); err != nil {
		if errors.Is(err, ErrPreviewDraining) {
			h.unavailable(w, r, "This host is being taken out of service",
				"The preview cannot start here. Try again shortly.")
			return
		}
		h.log.Warn("could not start preview", "preview", p.ID, "error", err)
	}

	h.unavailable(w, r, "Preparing environment",
		fmt.Sprintf("Starting a preview of %s. This takes a minute or two on a first boot.", p.Ref))
}

// unavailable writes the holding page or a 503, depending on what the client
// can make sense of.
func (h *previewIngress) unavailable(w http.ResponseWriter, r *http.Request, title, detail string) {
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("Retry-After", fmt.Sprintf("%d", previewRetryAfter))
	w.Header().Set("Cache-Control", "no-store")

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
	Error string `json:"error,omitempty"`
	Ready bool   `json:"ready"`
}

// writeStatus answers the holding page's poll with the live boot phase.
func (h *previewIngress) writeStatus(w http.ResponseWriter, r *http.Request, p *preview.Preview) {
	status := previewStatus{
		State: string(p.State),
		Error: p.Error,
		Ready: h.svc.previews.IsLive(p.ID),
	}
	status.Phase = h.bootPhase(r.Context(), p)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
	_ = json.NewEncoder(w).Encode(status)
}

// bootPhase reads the human-readable phase off the boot's session, falling back
// to the preview's own state when there is no session to read.
func (h *previewIngress) bootPhase(ctx context.Context, p *preview.Preview) string {
	if p.SessionID == "" || h.svc.sessionMgr == nil {
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

// proxy forwards the request into the guest.
func (h *previewIngress) proxy(w http.ResponseWriter, r *http.Request, p *preview.Preview, app preview.App) {
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("%s:%d", preview.NormalizeHost(app.Host), app.Port)}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// The app sees the hostname the browser asked for, not the
			// synthetic one the transport dials: absolute URLs it generates
			// have to match what is in the address bar.
			pr.Out.Host = r.Host
			pr.SetXForwarded()
			// TLS terminated in front of us, so the app cannot see the scheme
			// from the connection and has to be told.
			pr.Out.Header.Set("X-Forwarded-Proto", forwardedProto(r))
		},
		Transport: &http.Transport{
			// Every connection goes through the preview's own netstack; the
			// address in the URL exists only to satisfy the URL type.
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				dialCtx, cancel := context.WithTimeout(ctx, previewDialTimeout)
				defer cancel()
				conn, live, err := h.svc.previews.DialGuest(dialCtx, p.ID, app.Port)
				if err != nil {
					return nil, err
				}
				if !live {
					return nil, fmt.Errorf("preview %s is not running", p.ID)
				}
				return conn, nil
			},
			// A preview runs one small server behind a userspace network, so
			// the defaults sized for a public-internet client pool are wrong
			// here in both directions.
			MaxIdleConns:          16,
			IdleConnTimeout:       90 * time.Second,
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
			h.log.Warn("preview upstream failed", "preview", p.ID, "app", app.Name, "error", err)
			w.Header().Set("X-Robots-Tag", "noindex")
			if !wantsHTML(r) {
				http.Error(w, "preview upstream unavailable", http.StatusBadGateway)
				return
			}
			writeHTML(w, http.StatusBadGateway, previewErrorPage(
				"The app is not answering",
				fmt.Sprintf("The %s service in this preview did not respond.", app.Name),
				"Check kvarn preview logs for what it printed."))
		},
		ErrorLog: slog.NewLogLogger(h.log.Handler(), slog.LevelDebug),
	}

	rp.ServeHTTP(w, r)
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
  function poll() {
    fetch(%[3]q, { headers: { "Accept": "application/json" }, cache: "no-store" })
      .then(function (r) { return r.json(); })
      .then(function (s) {
        if (s.ready) { location.reload(); return; }
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
`, html.EscapeString(title), html.EscapeString(detail), previewStatusPath)
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
