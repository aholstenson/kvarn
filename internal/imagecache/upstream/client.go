// Package upstream resolves and talks to public OCI registries on behalf of
// the pull-through cache. Only anonymous pulls are supported in v1 — the
// docker.io / ghcr.io / quay.io / gcr.io token dances all work without
// credentials for public content.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client talks to upstream OCI registries with a per-host token cache. One
// instance is shared across the cache server; all per-host state is keyed by
// the configured upstream name.
type Client struct {
	HTTP *http.Client

	// Scheme overrides the URL scheme used for upstream fetches. Defaults
	// to "https"; tests set "http" so they can stand up plain httptest
	// servers without certificate plumbing.
	Scheme string

	// Hosts overrides the upstream-name → registry-host mapping returned
	// by ResolveHost. Useful for tests that need to point a configured
	// upstream like "docker.io" at a local httptest address. Entries not
	// present here fall back to ResolveHost.
	Hosts map[string]string

	mu     sync.Mutex
	tokens map[tokenKey]cachedToken
}

type tokenKey struct {
	host  string
	scope string
}

type cachedToken struct {
	value   string
	expires time.Time
}

// New returns a Client backed by a shared http.Client with sensible
// timeouts. Per-host connection pooling lives in the transport.
func New() *Client {
	return &Client{
		HTTP: &http.Client{
			Timeout: 5 * time.Minute,
		},
		tokens: make(map[tokenKey]cachedToken),
	}
}

// ResolveHost maps a configured upstream name to its registry hostname.
// "docker.io" is a virtual alias — Docker Hub's API actually lives at
// registry-1.docker.io, but the manifest path uses "library/<image>" for
// official images. The handler is responsible for normalizing repo names;
// this returns only the hostname.
func ResolveHost(upstream string) string {
	if upstream == "docker.io" {
		return "registry-1.docker.io"
	}
	return upstream
}

// NormalizeName applies the docker.io convention of prepending "library/" to
// single-segment names (e.g. "python" → "library/python"). Other registries
// pass through unchanged.
func NormalizeName(upstream, name string) string {
	if upstream != "docker.io" {
		return name
	}
	if strings.Contains(name, "/") {
		return name
	}
	return "library/" + name
}

// ManifestResponse is what the proxy needs from an upstream manifest fetch.
type ManifestResponse struct {
	Body        []byte
	ContentType string
	Digest      string
	ETag        string
	MaxAge      time.Duration
	StatusCode  int
}

// GetManifest fetches a manifest for (upstream, name, ref). If ifNoneMatch
// is non-empty it is sent as the If-None-Match header so the registry can
// reply with 304 Not Modified — the caller can then keep its cached body.
func (c *Client) GetManifest(ctx context.Context, upstream, name, ref, ifNoneMatch string) (*ManifestResponse, error) {
	host := c.resolveHost(upstream)
	repo := NormalizeName(upstream, name)
	u := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", c.scheme(), host, repo, url.PathEscape(ref))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	// Accept the union of manifest types we may legitimately encounter.
	// Listing each explicitly nudges multi-arch registries into returning
	// the manifest index rather than a single arch's manifest.
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v1+json",
	}, ", "))
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	return c.doManifest(ctx, upstream, repo, req)
}

func (c *Client) doManifest(ctx context.Context, upstream, repo string, req *http.Request) (*ManifestResponse, error) {
	resp, err := c.doAuthed(ctx, upstream, repo, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return &ManifestResponse{StatusCode: resp.StatusCode}, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read manifest body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: body}
	}
	return &ManifestResponse{
		Body:        body,
		ContentType: resp.Header.Get("Content-Type"),
		Digest:      resp.Header.Get("Docker-Content-Digest"),
		ETag:        resp.Header.Get("ETag"),
		MaxAge:      parseMaxAge(resp.Header.Get("Cache-Control")),
		StatusCode:  resp.StatusCode,
	}, nil
}

// UpstreamError carries a non-success status from the registry. The cache
// proxy uses StatusCode to surface 404s/401s back to the puller verbatim,
// and Body to mirror the registry's JSON error so the client gets a
// recognizable failure message.
type UpstreamError struct {
	StatusCode int
	Body       []byte
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream status %d: %s", e.StatusCode, string(e.Body))
}

// BlobResponse streams a blob from the upstream. The caller MUST close Body.
type BlobResponse struct {
	Body          io.ReadCloser
	ContentType   string
	ContentLength int64
	StatusCode    int
}

// GetBlob fetches a blob by digest from (upstream, name).
func (c *Client) GetBlob(ctx context.Context, upstream, name, digest string) (*BlobResponse, error) {
	host := c.resolveHost(upstream)
	repo := NormalizeName(upstream, name)
	u := fmt.Sprintf("%s://%s/v2/%s/blobs/%s", c.scheme(), host, repo, url.PathEscape(digest))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.doAuthed(ctx, upstream, repo, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		resp.Body.Close()
		return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: body}
	}
	return &BlobResponse{
		Body:          resp.Body,
		ContentType:   resp.Header.Get("Content-Type"),
		ContentLength: resp.ContentLength,
		StatusCode:    resp.StatusCode,
	}, nil
}

func (c *Client) scheme() string {
	if c.Scheme != "" {
		return c.Scheme
	}
	return "https"
}

func (c *Client) resolveHost(upstream string) string {
	if h, ok := c.Hosts[upstream]; ok && h != "" {
		return h
	}
	return ResolveHost(upstream)
}

// doAuthed sends req with bearer-token retry: if the registry replies 401
// with a Www-Authenticate challenge, the client fetches a token, caches it
// per (host, scope), and replays the request once.
func (c *Client) doAuthed(ctx context.Context, upstream, repo string, req *http.Request) (*http.Response, error) {
	host := c.resolveHost(upstream)
	scope := fmt.Sprintf("repository:%s:pull", repo)
	if tok := c.cachedToken(host, scope); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	challenge := resp.Header.Get("Www-Authenticate")
	resp.Body.Close()
	tok, err := c.fetchToken(ctx, challenge, host, scope)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	// Replay request with the freshly acquired token. Cloning preserves the
	// original method, URL, and headers.
	retry := req.Clone(ctx)
	retry.Header.Set("Authorization", "Bearer "+tok)
	return c.HTTP.Do(retry)
}

func (c *Client) cachedToken(host, scope string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tokens[tokenKey{host: host, scope: scope}]
	if !ok {
		return ""
	}
	if time.Now().After(t.expires) {
		delete(c.tokens, tokenKey{host: host, scope: scope})
		return ""
	}
	return t.value
}

func (c *Client) storeToken(host, scope, token string, expiresIn time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Knock 30s off the TTL so we don't race the expiry on the wire.
	if expiresIn > 30*time.Second {
		expiresIn -= 30 * time.Second
	}
	c.tokens[tokenKey{host: host, scope: scope}] = cachedToken{
		value:   token,
		expires: time.Now().Add(expiresIn),
	}
}

// parseChallenge extracts the realm, service, and scope params from a Bearer
// challenge of the form: Bearer realm="...",service="...",scope="..."
func parseChallenge(s string) map[string]string {
	out := map[string]string{}
	const prefix = "Bearer "
	if !strings.HasPrefix(s, prefix) {
		return out
	}
	s = s[len(prefix):]
	for len(s) > 0 {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			break
		}
		key := strings.TrimSpace(s[:eq])
		s = s[eq+1:]
		if !strings.HasPrefix(s, `"`) {
			break
		}
		s = s[1:]
		end := strings.IndexByte(s, '"')
		if end < 0 {
			break
		}
		out[key] = s[:end]
		s = s[end+1:]
		if strings.HasPrefix(s, ",") {
			s = s[1:]
		}
	}
	return out
}

func (c *Client) fetchToken(ctx context.Context, challenge, host, defaultScope string) (string, error) {
	params := parseChallenge(challenge)
	realm := params["realm"]
	if realm == "" {
		return "", errors.New("missing realm in challenge")
	}
	scope := params["scope"]
	if scope == "" {
		scope = defaultScope
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("parse realm: %w", err)
	}
	q := u.Query()
	if svc := params["service"]; svc != "" {
		q.Set("service", svc)
	}
	q.Set("scope", scope)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return "", fmt.Errorf("token endpoint status %d: %s", resp.StatusCode, body)
	}
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}
	tok := payload.Token
	if tok == "" {
		tok = payload.AccessToken
	}
	if tok == "" {
		return "", errors.New("empty token from registry")
	}
	exp := time.Duration(payload.ExpiresIn) * time.Second
	if exp == 0 {
		// Per the docker token spec, omitted expires_in is 60s.
		exp = 60 * time.Second
	}
	c.storeToken(host, scope, tok, exp)
	return tok, nil
}

// parseMaxAge returns the max-age directive from a Cache-Control header, or
// 0 if absent or unparseable.
func parseMaxAge(h string) time.Duration {
	for _, part := range strings.Split(h, ",") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "max-age=") {
			continue
		}
		v := strings.TrimPrefix(part, "max-age=")
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	return 0
}
