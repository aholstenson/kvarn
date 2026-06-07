package proxy

import (
	"bytes"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// SecretInjector enriches an outbound proxied HTTP request with credentials
// for the destination host. Implementations must not block longer than the
// caller's context allows. Inject may replace req.Body and adjust
// req.ContentLength (e.g. an oauth body rewrite); callers forwarding the
// request must use the post-Inject body and length.
type SecretInjector interface {
	Inject(req *http.Request, host string) error
}

// InjectorFunc adapts a plain function to the SecretInjector interface.
type InjectorFunc func(req *http.Request, host string) error

func (f InjectorFunc) Inject(req *http.Request, host string) error {
	return f(req, host)
}

// Scheme selects how a managed secret is applied to an outbound request.
// New schemes (e.g. request signing) extend this enum without reworking the
// SecretInjector seam.
type Scheme string

const (
	// SchemeBearer substitutes the placeholder verbatim in header values
	// (e.g. `Authorization: Bearer <placeholder>`).
	SchemeBearer Scheme = "bearer"
	// SchemeBasic decodes an HTTP Basic auth blob, substitutes inside the
	// decoded `user:pass`, and re-encodes.
	SchemeBasic Scheme = "basic"
	// SchemeOAuth substitutes the placeholder in the request body (e.g. a
	// form-encoded or JSON token-exchange POST).
	SchemeOAuth Scheme = "oauth"
)

// ManagedSecret describes a managed secret the proxy applies: the real value
// to substitute for the per-job placeholder, the scheme governing how it is
// applied, and the host patterns it is scoped to (empty = any allowlisted
// host).
type ManagedSecret struct {
	Value  string
	Scheme Scheme
	Hosts  []string
}

// maxOAuthBodyBytes caps body buffering for oauth substitution. Token-exchange
// bodies are tiny; larger requests (uploads) stream through untouched.
const maxOAuthBodyBytes = 64 * 1024

// placeholderEntry is a precomputed managed secret ready for matching.
type placeholderEntry struct {
	placeholder string
	value       string
	scheme      Scheme
	// allow scopes substitution to specific hosts; nil permits any host.
	allow *Allowlist
}

func (e *placeholderEntry) permit(host string) bool {
	if e.allow == nil {
		return true
	}
	return e.allow.Permit(host)
}

// PlaceholderInjector substitutes per-job placeholder strings in outbound
// requests with their real secret values. Placeholders are unguessable random
// tokens (kvarn_<32 hex chars>) injected into the VM as env-var values; the
// proxy here is what makes them functional credentials.
//
// Substitution is scheme-dispatched: bearer rewrites header values verbatim,
// basic decodes/re-encodes an HTTP Basic blob, and oauth rewrites the request
// body. A secret is only matched under its own scheme (a bearer secret is
// never decoded out of a Basic blob and vice-versa) — explicit and
// leak-narrowing. Each secret may further be scoped to a set of hosts.
type PlaceholderInjector struct {
	entries []placeholderEntry
	log     *slog.Logger
}

// NewPlaceholderInjector returns an injector for the given placeholder→secret
// pairs. The map is consumed at construction; callers may mutate it after.
func NewPlaceholderInjector(secrets map[string]ManagedSecret, log *slog.Logger) *PlaceholderInjector {
	if log == nil {
		log = slog.Default()
	}
	entries := make([]placeholderEntry, 0, len(secrets))
	for ph, ms := range secrets {
		scheme := ms.Scheme
		if scheme == "" {
			scheme = SchemeBearer
		}
		e := placeholderEntry{placeholder: ph, value: ms.Value, scheme: scheme}
		if len(ms.Hosts) > 0 {
			e.allow = NewAllowlist(ms.Hosts)
		}
		entries = append(entries, e)
	}
	return &PlaceholderInjector{entries: entries, log: log}
}

func (p *PlaceholderInjector) Inject(req *http.Request, host string) error {
	if len(p.entries) == 0 {
		return nil
	}

	// Select placeholders whose host scope permits this host.
	selected := make([]placeholderEntry, 0, len(p.entries))
	for _, e := range p.entries {
		if e.permit(host) {
			selected = append(selected, e)
		}
	}
	if len(selected) == 0 {
		return nil
	}

	// Header pass: bearer and basic schemes act on header values.
	for name, values := range req.Header {
		for i, v := range values {
			replaced := p.rewriteHeader(v, selected, host, name)
			if replaced != v {
				values[i] = replaced
			}
		}
	}

	// Body pass: oauth substitutes in the request body.
	return p.rewriteBody(req, selected, host)
}

func (p *PlaceholderInjector) rewriteHeader(v string, selected []placeholderEntry, host, header string) string {
	for _, e := range selected {
		switch e.scheme {
		case SchemeBearer:
			if strings.Contains(v, e.placeholder) {
				v = strings.ReplaceAll(v, e.placeholder, e.value)
				p.log.Debug("substituted bearer placeholder", "host", host, "header", header)
			}
		case SchemeBasic:
			v = p.rewriteBasic(v, e, host, header)
		}
	}
	return v
}

// rewriteBasic substitutes inside an HTTP Basic auth header value. The client
// base64-encodes "user:secret" into `Authorization: Basic <blob>`, so the raw
// placeholder is not visible in the header value — it must be decoded first.
func (p *PlaceholderInjector) rewriteBasic(v string, e placeholderEntry, host, header string) string {
	const prefix = "Basic "
	if len(v) < len(prefix) || !strings.EqualFold(v[:len(prefix)], prefix) {
		return v
	}
	blob := strings.TrimSpace(v[len(prefix):])
	decoded, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		return v
	}
	if !strings.Contains(string(decoded), e.placeholder) {
		return v
	}
	replaced := strings.ReplaceAll(string(decoded), e.placeholder, e.value)
	p.log.Debug("substituted basic placeholder", "host", host, "header", header)
	// Preserve the original prefix casing/spacing.
	return v[:len(prefix)] + base64.StdEncoding.EncodeToString([]byte(replaced))
}

// rewriteBody substitutes oauth placeholders in the request body. It only
// buffers when at least one selected placeholder is oauth and the body is
// within the cap; otherwise the body streams through untouched.
func (p *PlaceholderInjector) rewriteBody(req *http.Request, selected []placeholderEntry, host string) error {
	hasOAuth := false
	for _, e := range selected {
		if e.scheme == SchemeOAuth {
			hasOAuth = true
			break
		}
	}
	if !hasOAuth || req.Body == nil || req.Body == http.NoBody {
		return nil
	}
	// Known-oversized bodies stream through without buffering.
	if req.ContentLength > maxOAuthBodyBytes {
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, maxOAuthBodyBytes+1))
	req.Body.Close()
	if err != nil {
		return err
	}
	if int64(len(body)) > maxOAuthBodyBytes {
		// Body exceeded the cap (ContentLength was unknown). Restore the bytes
		// and pass through without substitution.
		req.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}

	changed := false
	for _, e := range selected {
		if e.scheme != SchemeOAuth {
			continue
		}
		if bytes.Contains(body, []byte(e.placeholder)) {
			body = bytes.ReplaceAll(body, []byte(e.placeholder), []byte(e.value))
			changed = true
			p.log.Debug("substituted oauth placeholder in body", "host", host)
		}
	}

	req.Body = io.NopCloser(bytes.NewReader(body))
	if changed {
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	return nil
}
