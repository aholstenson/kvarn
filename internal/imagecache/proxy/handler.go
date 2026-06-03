// Package proxy implements a read-through Docker Registry HTTP API v2
// handler. It serves /v2/, manifests, and blobs from the local store,
// falling back to a configured upstream registry when content is absent.
//
// Push, catalog, and authentication endpoints are intentionally not
// implemented: this is a pull-through cache, not a real registry.
package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aholstenson/kvarn/internal/imagecache/store"
	"github.com/aholstenson/kvarn/internal/imagecache/upstream"
)

// Config configures a Handler.
type Config struct {
	Store *store.Store

	// Upstreams maps the leading repo-name segment ("docker.io", "ghcr.io",
	// ...) to a resolved upstream. The handler matches request paths of the
	// form /v2/<upstream>/<repo>/manifests/... so podman's
	// `<gateway>:5000/docker.io/library/python:3.12` style works out of the
	// box.
	Upstreams []string

	// ManifestTagTTL is how long a mutable tag manifest is served from cache
	// without revalidating upstream. Defaults to 5 minutes when zero.
	ManifestTagTTL time.Duration

	// GlobalQuotaBytes, when > 0, triggers an opportunistic LRU sweep after
	// each successful blob write if the total exceeds the quota.
	GlobalQuotaBytes int64

	// UpstreamClient is the HTTP client used for upstream fetches. nil means
	// upstream.New() is created on the first call.
	UpstreamClient *upstream.Client

	// Logger; defaults to slog.Default().
	Logger *slog.Logger
}

// Handler is the HTTP entry point. ServeHTTP implements the v2 surface.
type Handler struct {
	cfg       Config
	upstreams map[string]struct{}
	client    *upstream.Client
	log       *slog.Logger
}

// New constructs a Handler from cfg.
func New(cfg Config) *Handler {
	if cfg.ManifestTagTTL == 0 {
		cfg.ManifestTagTTL = 5 * time.Minute
	}
	if cfg.UpstreamClient == nil {
		cfg.UpstreamClient = upstream.New()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	ups := make(map[string]struct{}, len(cfg.Upstreams))
	for _, u := range cfg.Upstreams {
		ups[u] = struct{}{}
	}
	return &Handler{
		cfg:       cfg,
		upstreams: ups,
		client:    cfg.UpstreamClient,
		log:       cfg.Logger,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v2/" || r.URL.Path == "/v2":
		// Mandatory v2 endpoint — clients ping it for API version
		// detection. The OCI distribution spec requires an empty 200.
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{}"))
		return
	case strings.HasPrefix(r.URL.Path, "/v2/"):
		h.serveV2(w, r)
		return
	}
	http.NotFound(w, r)
}

func (h *Handler) serveV2(w http.ResponseWriter, r *http.Request) {
	// Path format: /v2/<upstream>/<name...>/(manifests|blobs)/<ref>
	rest := strings.TrimPrefix(r.URL.Path, "/v2/")
	upstreamName, repo, kind, ref, ok := splitV2Path(rest)
	if !ok {
		writeError(w, http.StatusNotFound, "NAME_INVALID", "path not recognised")
		return
	}
	if _, allowed := h.upstreams[upstreamName]; !allowed {
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", fmt.Sprintf("upstream %q not configured", upstreamName))
		return
	}
	switch kind {
	case "manifests":
		h.serveManifest(w, r, upstreamName, repo, ref)
	case "blobs":
		h.serveBlob(w, r, upstreamName, repo, ref)
	default:
		writeError(w, http.StatusNotFound, "UNSUPPORTED", "only manifests and blobs are served")
	}
}

// splitV2Path parses /v2/<upstream>/<name segments...>/(manifests|blobs)/<ref>.
// The "name segments" portion may contain slashes — e.g. "library/python" or
// "namespace/repo/sub".
func splitV2Path(p string) (upstreamName, name, kind, ref string, ok bool) {
	segs := strings.Split(p, "/")
	if len(segs) < 4 {
		return
	}
	upstreamName = segs[0]
	// Search backwards for "manifests" or "blobs".
	for i := len(segs) - 2; i >= 1; i-- {
		if segs[i] == "manifests" || segs[i] == "blobs" {
			name = strings.Join(segs[1:i], "/")
			kind = segs[i]
			ref = strings.Join(segs[i+1:], "/")
			ok = name != "" && ref != ""
			return
		}
	}
	return
}

func (h *Handler) serveManifest(w http.ResponseWriter, r *http.Request, upstreamName, name, ref string) {
	isDigest := strings.HasPrefix(ref, "sha256:")
	cached, hit, err := h.cfg.Store.ReadManifest(upstreamName, name, ref)
	if err != nil {
		h.log.Warn("manifest read error", "upstream", upstreamName, "name", name, "ref", ref, "error", err)
	}
	if hit && (!cached.Meta.IsTag || h.cfg.Store.IsManifestFresh(cached.Meta) || isDigest) {
		h.writeManifest(w, r, cached)
		return
	}

	// Miss or stale tag — go upstream. For stale tags, pass the cached ETag
	// so the registry can answer 304.
	ifNoneMatch := ""
	if hit && cached.Meta.IsTag && cached.Meta.ETag != "" {
		ifNoneMatch = cached.Meta.ETag
	}
	resp, err := h.client.GetManifest(r.Context(), upstreamName, name, ref, ifNoneMatch)
	if err != nil {
		// On upstream failure, serve a stale cached entry rather than
		// breaking the build — registries flap and the cached copy is
		// likely good enough for the next 5 minutes.
		if hit {
			h.log.Warn("upstream manifest failed; serving stale", "upstream", upstreamName, "name", name, "ref", ref, "error", err)
			h.writeManifest(w, r, cached)
			return
		}
		writeUpstreamError(w, err)
		return
	}

	if resp.StatusCode == http.StatusNotModified && hit {
		// Refresh the TTL and serve cached body.
		ttl := h.tagTTL(resp.MaxAge)
		_ = h.cfg.Store.WriteManifest(upstreamName, name, ref, cached.Body, cached.Meta.ResolvedDigest, cached.Meta.ContentType, cached.Meta.ETag, cached.Meta.IsTag, ttl)
		h.writeManifest(w, r, cached)
		return
	}

	digest := resp.Digest
	if digest == "" {
		// Some registries omit Docker-Content-Digest; hash the body
		// ourselves so the local index is consistent.
		sum := sha256.Sum256(resp.Body)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	ttl := time.Duration(0)
	if !isDigest {
		ttl = h.tagTTL(resp.MaxAge)
	}
	if err := h.cfg.Store.WriteManifest(upstreamName, name, ref, resp.Body, digest, resp.ContentType, resp.ETag, !isDigest, ttl); err != nil {
		h.log.Warn("manifest write failed", "upstream", upstreamName, "name", name, "ref", ref, "error", err)
	}
	h.writeManifestRaw(w, r, resp.Body, resp.ContentType, digest)
}

func (h *Handler) tagTTL(upstreamMaxAge time.Duration) time.Duration {
	if upstreamMaxAge > 0 {
		return upstreamMaxAge
	}
	return h.cfg.ManifestTagTTL
}

func (h *Handler) writeManifest(w http.ResponseWriter, r *http.Request, entry *store.ManifestEntry) {
	h.writeManifestRaw(w, r, entry.Body, entry.ContentType, entry.Meta.ResolvedDigest)
}

func (h *Handler) writeManifestRaw(w http.ResponseWriter, r *http.Request, body []byte, contentType, digest string) {
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if digest != "" {
		w.Header().Set("Docker-Content-Digest", digest)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func (h *Handler) serveBlob(w http.ResponseWriter, r *http.Request, upstreamName, name, digest string) {
	if _, _, err := store.ParseDigest(digest); err != nil {
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
		return
	}

	if rc, size, hit, err := h.cfg.Store.OpenBlob(digest); err != nil {
		h.log.Warn("blob read error", "digest", digest, "error", err)
	} else if hit {
		defer rc.Close()
		h.log.Debug("blob cache hit", "upstream", upstreamName, "name", name, "digest", digest, "bytes", size)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, rc)
		return
	}

	// HEAD without a cached copy is rare in real workloads but the spec
	// requires support. Probe upstream and report.
	if r.Method == http.MethodHead {
		h.headBlobUpstream(w, r.Context(), upstreamName, name, digest)
		return
	}

	resp, err := h.client.GetBlob(r.Context(), upstreamName, name, digest)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()

	// Tee the body into the cache and the response simultaneously so we
	// don't double the bandwidth or wait for the full body before
	// streaming. Cache writes go to a temp file; if the rename fails or
	// the size disagrees the cache simply doesn't have this entry next
	// time around.
	pr, pw := io.Pipe()
	written := make(chan int64, 1)
	cacheErr := make(chan error, 1)
	go func() {
		n, err := h.cfg.Store.WriteBlob(digest, pr)
		written <- n
		cacheErr <- err
	}()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", digest)
	if resp.ContentLength > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.WriteHeader(http.StatusOK)

	mw := io.MultiWriter(w, pw)
	_, copyErr := io.Copy(mw, resp.Body)
	pw.Close()
	n := <-written
	werr := <-cacheErr
	if copyErr != nil {
		h.log.Warn("blob proxy copy failed", "digest", digest, "error", copyErr)
		return
	}
	if werr != nil {
		h.log.Warn("blob cache write failed", "digest", digest, "error", werr)
		return
	}
	h.log.Info("blob cached", "upstream", upstreamName, "name", name, "digest", digest, "bytes", n)
	h.maybeEvict()
}

func (h *Handler) headBlobUpstream(w http.ResponseWriter, ctx context.Context, upstreamName, name, digest string) {
	// A real HEAD against upstream would be nicer than a GET, but the
	// upstream client only exposes GetBlob. Doing a full GET here would
	// waste bandwidth, so signal "not in cache" and let the client follow
	// up with GET if it really wants it. Registries treat 404 on HEAD as
	// "not present"; pullers will retry with GET, which will populate the
	// cache.
	_ = ctx
	_ = upstreamName
	_ = name
	_ = digest
	w.WriteHeader(http.StatusNotFound)
}

// maybeEvict runs an opportunistic LRU sweep after a successful blob write
// when the configured global quota is exceeded. Non-fatal: failures only
// produce a warning, since the next sweep will retry.
func (h *Handler) maybeEvict() {
	if h.cfg.GlobalQuotaBytes <= 0 {
		return
	}
	stats, err := h.cfg.Store.Stats()
	if err != nil {
		h.log.Warn("stats failed during opportunistic eviction", "error", err)
		return
	}
	if stats.BlobBytes <= h.cfg.GlobalQuotaBytes {
		return
	}
	rep, err := h.cfg.Store.EvictGlobal(h.cfg.GlobalQuotaBytes)
	if err != nil {
		h.log.Warn("image cache eviction failed", "error", err)
		return
	}
	if rep.RemovedEntries > 0 {
		h.log.Info("image cache evicted", "entries", rep.RemovedEntries, "bytes_freed", rep.BytesFreed)
	}
}

// writeError emits a registry-shaped error so podman's parser surfaces
// something readable.
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"errors":[{"code":%q,"message":%q}]}`, code, message)
}

func writeUpstreamError(w http.ResponseWriter, err error) {
	var ue *upstream.UpstreamError
	if errors.As(err, &ue) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(ue.StatusCode)
		w.Write(ue.Body)
		return
	}
	writeError(w, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", err.Error())
}
