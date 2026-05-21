package proxy

import (
	"net/url"
	"strings"
	"sync"
)

// DefaultAllowedHosts contains hosts that every job is allowed to reach.
var DefaultAllowedHosts = []string{
	// Source hosting & Git
	"github.com",
	"gitlab.com",
	"bitbucket.org",
	"api.github.com",
	"codeload.github.com",
	"objects.githubusercontent.com",
	"raw.githubusercontent.com",

	// Container registries
	"registry-1.docker.io",
	"auth.docker.io",
	"production.cloudflare.docker.com",
	"production.cloudfront.docker.com",
	"*.r2.cloudflarestorage.com",
	"ghcr.io",
	"quay.io",

	// Debian (VM's own package manager)
	"deb.debian.org",
	"security.debian.org",
}

// Allowlist matches hostnames against an exact set and a wildcard suffix set.
// Wildcard entries take the form "*.example.com" and match any single or
// multi-label subdomain (e.g. "foo.example.com" or "a.b.example.com").
type Allowlist struct {
	mu        sync.RWMutex
	exact     map[string]struct{}
	wildcards []string // suffix without leading "*", i.e. ".example.com"
}

// NewAllowlist builds an Allowlist from the given host patterns.
func NewAllowlist(hosts []string) *Allowlist {
	a := &Allowlist{exact: make(map[string]struct{})}
	for _, h := range hosts {
		a.add(h)
	}
	return a
}

func (a *Allowlist) add(host string) {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return
	}
	if strings.HasPrefix(host, "*.") {
		a.wildcards = append(a.wildcards, host[1:]) // ".example.com"
		return
	}
	a.exact[host] = struct{}{}
}

// Add adds a host to the allowlist at runtime.
func (a *Allowlist) Add(host string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.add(host)
}

// Permit reports whether the given hostname is allowed.
func (a *Allowlist) Permit(host string) bool {
	if a == nil {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	if _, ok := a.exact[host]; ok {
		return true
	}
	for _, suffix := range a.wildcards {
		if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
			return true
		}
	}
	return false
}

// HostsFromMirrors extracts the host portion from a list of registry mirror
// URLs. Used by orchestrator wiring.
func HostsFromMirrors(mirrors []string) []string {
	var out []string
	for _, m := range mirrors {
		if u, err := url.Parse(m); err == nil && u.Hostname() != "" {
			out = append(out, u.Hostname())
		} else if m != "" {
			out = append(out, m)
		}
	}
	return out
}
