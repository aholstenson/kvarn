package preview

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Auto-start needs the opposite of what produces a preview hostname. A boot
// expands a pattern into a name; here a name has to expand back into what to
// boot, before any repository has been cloned and therefore before kvarn.yml is
// readable. That is why these patterns live in the operator's project config
// rather than in the repository: the mapping has to be known to answer the
// first request for a hostname nothing has claimed yet.
//
// Only `{pr}` is matchable. `{ref}` cannot be: RefLabel deliberately slugs and
// digests a ref into one label, and nothing recovers `feat/login` from
// `feat-login-a1b2c3`.

// ErrNoRoute is returned by a Router that claims nothing for a hostname.
var ErrNoRoute = errors.New("no auto-start route claims this hostname")

// maxPRLabel bounds the matched pull request identifier. It is one DNS label at
// most, and a bound here keeps a long hostname from becoming a long forge API
// path.
const maxPRLabel = 63

// Route is one project's claim on a family of hostnames. It is a compiled
// prefix/suffix pair rather than a regular expression: the pattern has exactly
// one variable, and matching by string bounds what an unauthenticated request
// for an arbitrary hostname can cost.
type Route struct {
	// Project is the project a matching hostname belongs to.
	Project string
	// Pattern is the configured spelling, kept for error messages.
	Pattern string

	prefix string
	suffix string
}

// Match reports the pull request a hostname names under this route.
func (r Route) Match(host string) (string, bool) {
	host = NormalizeHost(host)
	if len(host) <= len(r.prefix)+len(r.suffix) {
		return "", false
	}
	if !strings.HasPrefix(host, r.prefix) || !strings.HasSuffix(host, r.suffix) {
		return "", false
	}
	pr := host[len(r.prefix) : len(host)-len(r.suffix)]
	if !isPRLabelPart(pr) {
		return "", false
	}
	return pr, true
}

// isPRLabelPart reports whether the matched text can be a pull request
// identifier sitting inside one hostname label. A dot would mean the pattern
// matched across a label boundary, which would let `evil.pr-1` reach the route
// for `pr-1`.
func isPRLabelPart(s string) bool {
	if s == "" || len(s) > maxPRLabel {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-':
		default:
			return false
		}
	}
	return true
}

// CompileRoute turns one configured pattern into a matcher for a project whose
// preview hostnames sit under domain.
func CompileRoute(project, pattern, domain string) (Route, error) {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" {
		return Route{}, errors.New("is empty")
	}
	domain = strings.ToLower(strings.Trim(strings.TrimSpace(domain), "."))
	if domain == "" {
		return Route{}, errors.New("cannot be matched: no preview domain is configured")
	}
	if strings.Contains(pattern, "://") || strings.Contains(pattern, "/") {
		return Route{}, errors.New("must be a hostname, without a scheme or a path")
	}

	// Same containment rule the repository's own patterns obey: a name that does
	// not end in the configured domain is the config claiming a hostname in a
	// zone previews do not own.
	if pattern != "{domain}" && !strings.HasSuffix(pattern, ".{domain}") {
		return Route{}, errors.New("must end in .{domain} so the name stays inside the preview domain")
	}

	if strings.Count(pattern, "{pr}") != 1 {
		// Zero would claim one fixed hostname, which auto-start has no use for;
		// more than one cannot be matched unambiguously.
		return Route{}, errors.New("must use {pr} exactly once")
	}
	expanded := strings.ReplaceAll(pattern, "{domain}", domain)
	if rest := strings.ReplaceAll(expanded, "{pr}", ""); strings.Contains(rest, "{") || strings.Contains(rest, "}") {
		return Route{}, errors.New("uses a placeholder other than {pr} and {domain}")
	}

	prefix, suffix, _ := strings.Cut(expanded, "{pr}")
	// `{pr}` must share its label with something literal. On its own it would be
	// a whole label, and the route would then claim every name in the zone —
	// including ones a repository's own `{ref}` sites answer on.
	if strings.HasSuffix(prefix, ".") || prefix == "" {
		return Route{}, errors.New("must give {pr} a literal prefix in its own label, such as pr-{pr}")
	}

	return Route{Project: project, Pattern: pattern, prefix: prefix, suffix: suffix}, nil
}

// Match is what a hostname resolved to.
type Match struct {
	Project string
	PR      string
	// Pattern is the route that claimed the hostname, for the error a boot
	// raises when the repository's own sites do not produce the same name.
	Pattern string
}

// Router matches a hostname against every configured route.
type Router struct {
	routes []Route
}

// NewRouter orders routes so matching does not depend on the order projects
// happened to be listed in, and refuses two projects claiming one family of
// names — an ambiguity that would otherwise show up as a preview of the wrong
// repository.
func NewRouter(routes []Route) (*Router, error) {
	ordered := make([]Route, len(routes))
	copy(ordered, routes)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Project != ordered[j].Project {
			return ordered[i].Project < ordered[j].Project
		}
		return ordered[i].Pattern < ordered[j].Pattern
	})

	seen := make(map[string]string, len(ordered))
	for _, r := range ordered {
		key := r.prefix + "\x00" + r.suffix
		if other, dup := seen[key]; dup && other != r.Project {
			return nil, fmt.Errorf("projects %q and %q both claim %q for preview auto-start",
				other, r.Project, r.Pattern)
		}
		seen[key] = r.Project
	}
	return &Router{routes: ordered}, nil
}

// Match resolves a hostname, or reports ErrNoRoute.
func (r *Router) Match(host string) (Match, error) {
	if r == nil {
		return Match{}, ErrNoRoute
	}
	host = NormalizeHost(host)
	for _, route := range r.routes {
		if pr, ok := route.Match(host); ok {
			return Match{Project: route.Project, PR: pr, Pattern: route.Pattern}, nil
		}
	}
	return Match{}, ErrNoRoute
}

// Empty reports whether any project has auto-start configured at all, so the
// ingress can skip the whole path when none has.
func (r *Router) Empty() bool { return r == nil || len(r.routes) == 0 }
