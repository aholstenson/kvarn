// Package proxy implements a pull-through Nix binary cache: an HTTP handler
// that speaks the substituter protocol Nix uses against cache.nixos.org,
// serving narinfo records and NAR files from the local store and fetching
// what is absent from a configured upstream cache on the way through.
//
// The handler is reached from inside each VM at a path that names the
// upstream, e.g. http://<gateway>:<port>/cache.nixos.org, which is what the
// guest lists as a substituter. Nix appends /nix-cache-info, /<hash>.narinfo
// and /nar/<file> to that base. Signatures are passed through untouched, so
// the guest verifies what it installs against the upstream's public key just
// as it would without the cache.
package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aholstenson/kvarn/internal/nixcache/store"
)

const (
	// defaultPriority is the priority the handler reports in nix-cache-info.
	// Nix orders substituters by this value, lowest first; cache.nixos.org
	// reports 40, so anything below that puts the host cache in front of it
	// no matter how the guest lists them.
	defaultPriority = 10

	// defaultNegativeTTL is how long a narinfo the upstream does not have is
	// remembered as absent. A path a job built itself is asked for by every
	// job on that project, and the answer will not change within a run.
	defaultNegativeTTL = 5 * time.Minute

	// maxNegativeEntries bounds the negative cache. Past it the map is reset
	// rather than grown; the cost of forgetting is one upstream round trip.
	maxNegativeEntries = 100_000

	// maxNarInfoBytes caps how much of an upstream narinfo response is read.
	// Real records are a few hundred bytes; the cap is what keeps an error
	// page or a misrouted response from being stored as one.
	maxNarInfoBytes = 64 * 1024

	contentTypeNarInfo   = "text/x-nix-narinfo"
	contentTypeNar       = "application/x-nix-nar"
	contentTypeCacheInfo = "text/x-nix-cache-info"
)

// Config configures a Handler.
type Config struct {
	Store *store.Store

	// Upstreams lists the binary caches to pull through, as URLs such as
	// "https://cache.nixos.org". Each is addressed by its hostname as the
	// first path segment of a request.
	Upstreams []string

	// GlobalQuotaBytes, when > 0, triggers an LRU sweep of the NAR files after
	// a write pushes the total past it.
	GlobalQuotaBytes int64

	// NegativeTTL is how long an upstream 404 for a narinfo is remembered.
	// Defaults to five minutes when zero.
	NegativeTTL time.Duration

	// Priority is the value reported in nix-cache-info. Defaults to 10.
	Priority int

	// HTTPClient is used for upstream fetches. nil means a client tuned for
	// many concurrent small requests and a few large streams.
	HTTPClient *http.Client

	// Logger; defaults to slog.Default().
	Logger *slog.Logger
}

// Handler is the HTTP entry point of the cache.
type Handler struct {
	cfg       Config
	upstreams map[string]*url.URL
	client    *http.Client
	log       *slog.Logger
	negative  *negativeCache
	narExpect *narExpectations

	cacheInfoMu sync.Mutex
	cacheInfo   map[string][]byte
}

// New constructs a Handler from cfg. An upstream whose URL does not parse or
// has no hostname is an error, because there would be nothing to route to.
func New(cfg Config) (*Handler, error) {
	if cfg.Store == nil {
		return nil, errors.New("nix cache: store is required")
	}
	if cfg.NegativeTTL == 0 {
		cfg.NegativeTTL = defaultNegativeTTL
	}
	if cfg.Priority == 0 {
		cfg.Priority = defaultPriority
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = newHTTPClient()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	ups := make(map[string]*url.URL, len(cfg.Upstreams))
	for _, raw := range cfg.Upstreams {
		u, err := url.Parse(strings.TrimRight(raw, "/"))
		if err != nil {
			return nil, fmt.Errorf("nix cache: upstream %q: %w", raw, err)
		}
		if u.Scheme == "" || u.Hostname() == "" {
			return nil, fmt.Errorf("nix cache: upstream %q must be a URL with a scheme and host", raw)
		}
		ups[u.Hostname()] = u
	}
	return &Handler{
		cfg:       cfg,
		upstreams: ups,
		client:    cfg.HTTPClient,
		log:       cfg.Logger,
		negative:  newNegativeCache(cfg.NegativeTTL),
		narExpect: newNarExpectations(),
		cacheInfo: make(map[string][]byte),
	}, nil
}

// newHTTPClient builds the upstream client. Compression is left to the
// bytes themselves: NAR files arrive already compressed and a narinfo is too
// small to matter, and an identity transfer is what keeps the upstream
// Content-Length meaningful for the response the guest sees. There is no
// overall timeout because a NAR can be gigabytes; the header timeout bounds
// the one hang that has no bytes flowing to detect it by.
func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			DisableCompression:    true,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}
}

// UpstreamHosts returns the hostnames requests are routed by, one per
// configured upstream.
func (h *Handler) UpstreamHosts() []string {
	out := make([]string, 0, len(h.upstreams))
	for host := range h.upstreams {
		out = append(out, host)
	}
	return out
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	host, rest, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	base, known := h.upstreams[host]
	if !known {
		http.Error(w, fmt.Sprintf("upstream %q not configured", host), http.StatusNotFound)
		return
	}
	switch {
	case rest == "nix-cache-info":
		h.serveCacheInfo(w, r, host, base)
	case strings.HasSuffix(rest, ".narinfo") && store.ValidStorePathHash(strings.TrimSuffix(rest, ".narinfo")):
		h.serveNarInfo(w, r, host, base, strings.TrimSuffix(rest, ".narinfo"))
	case strings.HasPrefix(rest, "nar/"):
		name := strings.TrimPrefix(rest, "nar/")
		if _, ok := store.ParseNarName(name); !ok {
			http.NotFound(w, r)
			return
		}
		h.serveNar(w, r, host, base, name)
	default:
		http.NotFound(w, r)
	}
}

// serveCacheInfo answers the first request Nix makes of a substituter. The
// upstream's record is fetched once and kept in memory with its Priority
// rewritten, so Nix sorts the host cache ahead of the upstream it fronts. An
// upstream that cannot be reached is reported as an error rather than
// papered over: Nix then skips this substituter with a warning and carries on
// with the next, which is the fallback the guest is configured with.
func (h *Handler) serveCacheInfo(w http.ResponseWriter, r *http.Request, host string, base *url.URL) {
	body, err := h.cachedCacheInfo(r, host, base)
	if err != nil {
		h.log.Warn("nix cache: upstream nix-cache-info failed", "upstream", host, "error", err)
		http.Error(w, "upstream unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeBytes(w, r, contentTypeCacheInfo, body)
}

func (h *Handler) cachedCacheInfo(r *http.Request, host string, base *url.URL) ([]byte, error) {
	h.cacheInfoMu.Lock()
	defer h.cacheInfoMu.Unlock()
	if body, ok := h.cacheInfo[host]; ok {
		return body, nil
	}
	resp, err := h.fetch(r, base, "nix-cache-info", "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxNarInfoBytes))
	if err != nil {
		return nil, err
	}
	body := rewritePriority(raw, h.cfg.Priority)
	h.cacheInfo[host] = body
	return body, nil
}

// rewritePriority replaces the Priority line of a nix-cache-info record, or
// appends one when the upstream did not send it.
func rewritePriority(info []byte, priority int) []byte {
	var out bytes.Buffer
	replaced := false
	for _, line := range strings.Split(strings.TrimRight(string(info), "\n"), "\n") {
		if strings.HasPrefix(line, "Priority:") {
			line = "Priority: " + strconv.Itoa(priority)
			replaced = true
		}
		if line == "" {
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if !replaced {
		fmt.Fprintf(&out, "Priority: %d\n", priority)
	}
	return out.Bytes()
}

func (h *Handler) serveNarInfo(w http.ResponseWriter, r *http.Request, host string, base *url.URL, hash string) {
	if body, hit, err := h.cfg.Store.ReadNarInfo(host, hash); err != nil {
		h.log.Warn("nix cache: narinfo read error", "upstream", host, "hash", hash, "error", err)
	} else if hit {
		h.rememberNar(body)
		writeBytes(w, r, contentTypeNarInfo, body)
		return
	}
	if h.negative.has(host + "/" + hash) {
		http.NotFound(w, r)
		return
	}

	// A HEAD that misses is answered by fetching the record: it is a few
	// hundred bytes, and the GET that follows a positive HEAD is then a hit.
	resp, err := h.fetch(r, base, hash+".narinfo", "")
	if err != nil {
		h.log.Warn("nix cache: upstream narinfo failed", "upstream", host, "hash", hash, "error", err)
		http.Error(w, "upstream unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case isAbsent(resp.StatusCode):
		h.negative.add(host + "/" + hash)
		http.NotFound(w, r)
		return
	default:
		h.log.Warn("nix cache: upstream narinfo error", "upstream", host, "hash", hash, "status", resp.StatusCode)
		http.Error(w, fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode), http.StatusBadGateway)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxNarInfoBytes+1))
	if err != nil {
		h.log.Warn("nix cache: upstream narinfo read failed", "upstream", host, "hash", hash, "error", err)
		http.Error(w, "upstream read failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if len(body) > maxNarInfoBytes || !bytes.HasPrefix(body, []byte("StorePath:")) {
		// Whatever this is, it is not a narinfo, and storing it would serve
		// it to every guest from now on.
		h.log.Warn("nix cache: upstream narinfo malformed", "upstream", host, "hash", hash, "bytes", len(body))
		http.Error(w, "upstream returned a malformed narinfo", http.StatusBadGateway)
		return
	}
	if err := h.cfg.Store.WriteNarInfo(host, hash, body); err != nil {
		h.log.Warn("nix cache: narinfo write failed", "upstream", host, "hash", hash, "error", err)
	}
	h.rememberNar(body)
	writeBytes(w, r, contentTypeNarInfo, body)
}

// rememberNar notes what the NAR this record points at must hash to, so the
// download that follows can be verified before it is kept.
func (h *Handler) rememberNar(narinfo []byte) {
	if name, fileHash, ok := parseNarExpectation(narinfo); ok {
		h.narExpect.put(name, fileHash)
	}
}

func (h *Handler) serveNar(w http.ResponseWriter, r *http.Request, host string, base *url.URL, name string) {
	if f, size, hit, err := h.cfg.Store.OpenNar(name); err != nil {
		h.log.Warn("nix cache: nar read error", "name", name, "error", err)
	} else if hit {
		defer f.Close()
		h.log.Debug("nix cache: nar hit", "upstream", host, "name", name, "bytes", size)
		w.Header().Set("Content-Type", contentTypeNar)
		// ServeContent answers HEAD and Range requests, which is how a Nix
		// download that broke off resumes from where it stopped.
		http.ServeContent(w, r, name, time.Time{}, f)
		return
	}

	// A HEAD or a ranged read of something not yet cached is passed through
	// as it is. Neither carries the whole file, so neither can fill the
	// cache; a later plain GET will.
	if r.Method == http.MethodHead || r.Header.Get("Range") != "" {
		h.passThrough(w, r, host, base, "nar/"+name)
		return
	}

	resp, err := h.fetch(r, base, "nar/"+name, "")
	if err != nil {
		h.log.Warn("nix cache: upstream nar failed", "upstream", host, "name", name, "error", err)
		http.Error(w, "upstream unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case isAbsent(resp.StatusCode):
		http.NotFound(w, r)
		return
	default:
		h.log.Warn("nix cache: upstream nar error", "upstream", host, "name", name, "status", resp.StatusCode)
		http.Error(w, fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode), http.StatusBadGateway)
		return
	}

	// Stream to the guest and into the store at once, so the guest waits on
	// the upstream transfer and nothing else. The store verifies the bytes
	// against the hash the narinfo declared for them before it keeps them; the
	// guest verifies them itself against the same record, so a bad upstream
	// transfer is caught on both sides and cached on neither.
	expect := h.narExpect.get(name)
	pr, pw := io.Pipe()
	type writeResult struct {
		n   int64
		err error
	}
	done := make(chan writeResult, 1)
	go func() {
		n, err := h.cfg.Store.WriteNar(name, expect, pr)
		// Drain what the copy may still push so it never blocks on a store
		// that has stopped reading.
		_, _ = io.Copy(io.Discard, pr)
		done <- writeResult{n, err}
	}()

	w.Header().Set("Content-Type", contentTypeNar)
	w.Header().Set("Accept-Ranges", "bytes")
	if resp.ContentLength > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.WriteHeader(http.StatusOK)

	_, copyErr := io.Copy(io.MultiWriter(w, pw), resp.Body)
	if copyErr != nil {
		pw.CloseWithError(copyErr)
	} else {
		pw.Close()
	}
	res := <-done
	switch {
	case copyErr != nil:
		h.log.Debug("nix cache: nar transfer ended early", "upstream", host, "name", name, "error", copyErr)
	case errors.Is(res.err, store.ErrHashMismatch) && expect == "":
		// No record naming this file has passed through, so there is nothing
		// to check the bytes against and they are streamed on unkept. Nix asks
		// for the record first, so this stays rare.
		h.log.Debug("nix cache: nar arrived without its narinfo; not cached", "upstream", host, "name", name, "bytes", res.n)
	case errors.Is(res.err, store.ErrHashMismatch):
		h.log.Warn("nix cache: upstream nar did not match the hash its narinfo declares; not cached", "upstream", host, "name", name, "bytes", res.n)
	case res.err != nil:
		h.log.Warn("nix cache: nar write failed", "upstream", host, "name", name, "error", res.err)
	default:
		h.log.Info("nix cache: nar cached", "upstream", host, "name", name, "bytes", res.n)
		h.maybeEvict()
	}
}

// passThrough relays one request to the upstream without touching the store.
func (h *Handler) passThrough(w http.ResponseWriter, r *http.Request, host string, base *url.URL, path string) {
	resp, err := h.fetch(r, base, path, r.Header.Get("Range"))
	if err != nil {
		h.log.Warn("nix cache: upstream request failed", "upstream", host, "path", path, "error", err)
		http.Error(w, "upstream unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, k := range []string{"Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set("Content-Type", contentTypeNar)
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, resp.Body)
	}
}

// fetch issues one upstream request on behalf of r, so the transfer stops
// when the guest gives up on it.
func (h *Handler) fetch(r *http.Request, base *url.URL, path, rangeHeader string) (*http.Response, error) {
	u := *base
	u.Path = strings.TrimRight(base.Path, "/") + "/" + path
	method := http.MethodGet
	if r.Method == http.MethodHead && rangeHeader == "" && strings.HasPrefix(path, "nar/") {
		method = http.MethodHead
	}
	req, err := http.NewRequestWithContext(r.Context(), method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "kvarn-nix-cache")
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	return h.client.Do(req)
}

// isAbsent reports whether an upstream status means "not here". Object
// stores behind some caches answer 403 rather than 404 for a missing key, and
// Nix treats both as a miss.
func isAbsent(status int) bool {
	return status == http.StatusNotFound || status == http.StatusForbidden
}

func writeBytes(w http.ResponseWriter, r *http.Request, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// maybeEvict runs an LRU sweep after a NAR write when the store has grown
// past the configured quota. A failure only logs: the next write tries again.
func (h *Handler) maybeEvict() {
	if h.cfg.GlobalQuotaBytes <= 0 {
		return
	}
	stats, err := h.cfg.Store.Stats()
	if err != nil {
		h.log.Warn("nix cache: stats failed before eviction", "error", err)
		return
	}
	if stats.NarBytes <= h.cfg.GlobalQuotaBytes {
		return
	}
	rep, err := h.cfg.Store.EvictGlobal(h.cfg.GlobalQuotaBytes)
	if err != nil {
		h.log.Warn("nix cache: eviction failed", "error", err)
		return
	}
	if rep.RemovedEntries > 0 {
		h.log.Info("nix cache: evicted", "entries", rep.RemovedEntries, "bytes_freed", rep.BytesFreed)
	}
}

// negativeCache remembers narinfo lookups the upstream answered 404 to, so
// the same miss from the next job costs no round trip for a while.
type negativeCache struct {
	ttl   time.Duration
	clock func() time.Time

	mu      sync.Mutex
	entries map[string]time.Time
}

func newNegativeCache(ttl time.Duration) *negativeCache {
	return &negativeCache{ttl: ttl, clock: time.Now, entries: make(map[string]time.Time)}
}

func (c *negativeCache) has(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	expires, ok := c.entries[key]
	if !ok {
		return false
	}
	if c.clock().After(expires) {
		delete(c.entries, key)
		return false
	}
	return true
}

func (c *negativeCache) add(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= maxNegativeEntries {
		c.entries = make(map[string]time.Time)
	}
	c.entries[key] = c.clock().Add(c.ttl)
}
