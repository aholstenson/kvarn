package proxy

import (
	"net/url"
	"slices"
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
	"raw.githubusercontent.com",
	// A release asset download is a 302 from github.com to one of these, and
	// the client follows it as a fresh connection with its own SNI. Allowing
	// github.com alone gets a redirect the client cannot follow.
	"objects.githubusercontent.com",
	"release-assets.githubusercontent.com",

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

// DefaultPorts are the ports a rule covers when it names none: the two the
// proxy reads HTTP on.
var DefaultPorts = []uint16{80, 443}

// Rule is one allowlist entry: a host pattern, the TCP ports it opens, and
// whether TLS on port 443 is terminated or spliced.
type Rule struct {
	// Host is a literal hostname, an IP address, or the "*.example.com"
	// wildcard form matching any single or multi-label subdomain.
	Host string
	// Ports are the TCP ports permitted to this host. Empty means DefaultPorts.
	Ports []uint16
	// Passthrough splices TLS on port 443 instead of terminating it, which
	// gives up secret injection in exchange for reaching a host that pins its
	// certificate or asks for a client one.
	Passthrough bool
}

// HostRules turns bare host patterns into rules on the default ports. It is how
// the built-in host lists — the defaults, a dependency's flake source, a
// registered tool's endpoints — enter an allowlist.
func HostRules(hosts []string) []Rule {
	if len(hosts) == 0 {
		return nil
	}
	rules := make([]Rule, 0, len(hosts))
	for _, h := range hosts {
		rules = append(rules, Rule{Host: h})
	}
	return rules
}

// Allowlist matches a hostname and port against an exact set and a wildcard
// suffix set. Wildcard entries take the form "*.example.com" and match any
// single or multi-label subdomain (e.g. "foo.example.com" or
// "a.b.example.com").
//
// Two entries may cover the same name — a wildcard and the exact host under it,
// or two rules opening different ports — so a lookup collects every match
// rather than stopping at the first. The permissions union: a host is reachable
// on any port any matching rule opens, and TLS is spliced when the rule that
// opened 443 asked for it.
type Allowlist struct {
	mu        sync.RWMutex
	exact     map[string][]Rule
	wildcards []wildcardRule // suffix without leading "*", i.e. ".example.com"
}

type wildcardRule struct {
	suffix string
	rule   Rule
}

// NewAllowlist builds an Allowlist from the given rules.
func NewAllowlist(rules []Rule) *Allowlist {
	a := &Allowlist{exact: make(map[string][]Rule)}
	for _, r := range rules {
		a.add(r)
	}
	return a
}

// NewHostAllowlist builds an Allowlist from bare host patterns, each on the
// default ports.
func NewHostAllowlist(hosts []string) *Allowlist {
	return NewAllowlist(HostRules(hosts))
}

func (a *Allowlist) add(rule Rule) {
	host := strings.TrimSpace(strings.ToLower(rule.Host))
	if host == "" {
		return
	}
	rule.Host = host
	if len(rule.Ports) == 0 {
		rule.Ports = DefaultPorts
	}
	if strings.HasPrefix(host, "*.") {
		a.wildcards = append(a.wildcards, wildcardRule{suffix: host[1:], rule: rule})
		return
	}
	a.exact[host] = append(a.exact[host], rule)
}

// Add adds a host to the allowlist at runtime, on the default ports.
func (a *Allowlist) Add(host string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.add(Rule{Host: host})
}

// matches collects every rule covering the hostname, most specific first.
func (a *Allowlist) matches(host string) []Rule {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	var out []Rule
	out = append(out, a.exact[host]...)
	for _, w := range a.wildcards {
		if strings.HasSuffix(host, w.suffix) && len(host) > len(w.suffix) {
			out = append(out, w.rule)
		}
	}
	return out
}

// Lookup returns the rule that admits host on port, if any. Where several
// rules open the port, one asking for passthrough wins: a host is named that
// way because inspecting it breaks, and that is true however else it is listed.
func (a *Allowlist) Lookup(host string, port uint16) (Rule, bool) {
	if a == nil {
		return Rule{}, false
	}
	var found Rule
	var ok bool
	for _, rule := range a.matches(host) {
		for _, p := range rule.Ports {
			if p != port {
				continue
			}
			if !ok || rule.Passthrough {
				found, ok = rule, true
			}
			break
		}
	}
	return found, ok
}

// Permit reports whether the given hostname is allowed on the given port.
func (a *Allowlist) Permit(host string, port uint16) bool {
	_, ok := a.Lookup(host, port)
	return ok
}

// PermitAny reports whether the hostname is allowed on any port at all. It is
// the question the secret injector asks: a secret is scoped to the hosts it may
// be sent to, and only an inspected request can carry one, so the port it
// arrived on adds nothing.
func (a *Allowlist) PermitAny(host string) bool {
	if a == nil {
		return false
	}
	return len(a.matches(host)) > 0
}

// ExtraPorts lists the ports the allowlist opens beyond the default two, in
// ascending order. The proxy binds one listener per entry, so this is what the
// VM's netstack has to be told before it boots.
func (a *Allowlist) ExtraPorts() []uint16 {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()

	seen := make(map[uint16]struct{})
	collect := func(rule Rule) {
		for _, p := range rule.Ports {
			if p == 80 || p == 443 {
				continue
			}
			seen[p] = struct{}{}
		}
	}
	for _, rules := range a.exact {
		for _, r := range rules {
			collect(r)
		}
	}
	for _, w := range a.wildcards {
		collect(w.rule)
	}

	ports := make([]uint16, 0, len(seen))
	for p := range seen {
		ports = append(ports, p)
	}
	slices.Sort(ports)
	return ports
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
