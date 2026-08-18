package project

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"errors"
)

// Preview is the `preview:` block of a kvarn.yml. It describes the shape of a
// preview environment for this repository: which hostnames are served from
// which ports, what to run to bring those ports up, and how to tell when they
// are ready.
//
// The operator owns the domain and the repository owns the shape. A host
// pattern here can only ever land inside the domain configured for the project,
// which is what keeps a branch from claiming a name in the operator's zone by
// editing this file.
type Preview struct {
	// Sites are the addresses this preview answers on, keyed by a short name.
	// A site is one hostname and the port behind it; several sites may name one
	// port when a single server answers under several names.
	Sites map[string]PreviewSite `yaml:"sites,omitempty"`
	// Serve are the long-lived commands that bring the ports up. Each names the
	// port it binds, and every port the sites name must be served by exactly
	// one — only one process can bind a port.
	Serve []PreviewProcess `yaml:"serve,omitempty"`
	// Ready are the checks that decide when the preview may take traffic. They
	// are ordinary steps, run in order, and retried until they pass or the boot
	// gives up.
	Ready []Step `yaml:"ready,omitempty"`
}

// PreviewSite is one hostname the preview answers on, and the port that answers
// it.
type PreviewSite struct {
	// Port is the guest port the server listens on. Several sites may name the
	// same port when one virtual-hosting server answers under several names;
	// what has to be unique is the hostname, since that is what routes.
	Port uint16 `yaml:"port"`
	// Host is the hostname pattern this site answers on. It may use `{ref}` for
	// the slugged git ref and `{domain}` for the project's configured preview
	// domain. Empty defaults to DefaultHostPattern.
	Host string `yaml:"host,omitempty"`
}

// PreviewProcess is one long-lived command that binds a port.
type PreviewProcess struct {
	Name       string   `yaml:"name"`
	Run        string   `yaml:"run"`
	WorkingDir string   `yaml:"working_dir,omitempty"`
	Port       uint16   `yaml:"port"`
	Env        []string `yaml:"env,omitempty"`
}

// DefaultHostPattern is what a site's `host` means when it is left out. It is
// the single-site answer: one preview, one name, derived from the ref.
const DefaultHostPattern = "{ref}.{domain}"

// MaxRefLabelLen is the DNS limit on a single label. A ref slug has to fit
// inside one, because the pattern places it in front of a dot and a slug that
// grew a dot of its own would silently claim a different name.
const MaxRefLabelLen = 63

// refLabelHashLen is how many base32 characters of the ref's digest are
// appended when the readable part had to be shortened or scrubbed. Six
// characters is 30 bits, which is far more than enough to keep the handful of
// branches one repository has open apart.
const refLabelHashLen = 6

// nonLabelChars matches runs of anything that cannot appear in a DNS label.
var nonLabelChars = regexp.MustCompile(`[^a-z0-9]+`)

// previewSiteNameRe constrains a site name to something usable as an env-var
// suffix, since each site's URL is exported as KVARN_PREVIEW_URL_<SITE>.
var previewSiteNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// hostPatternPlaceholder matches the `{name}` placeholders a host pattern may
// contain, so an unknown one can be rejected rather than left in the hostname.
var hostPatternPlaceholder = regexp.MustCompile(`\{([a-z_]*)\}`)

// Enabled reports whether the repository declares a preview at all.
func (p Preview) Enabled() bool { return len(p.Sites) > 0 }

// RefLabel turns a git ref into exactly one DNS label: lowercase, at most
// MaxRefLabelLen bytes, and free of any character that could change what the
// resulting hostname means.
//
// Refs contain slashes, uppercase and punctuation, and two different refs can
// easily reduce to the same readable text — `feat/login` and `feat-login` both
// want to be `feat-login`. Whenever the readable form is not a faithful
// rendering of the ref, a short digest of the original is appended, so the
// result stays deterministic, stays inside one label, and never collides.
func RefLabel(ref string) string {
	lowered := strings.ToLower(ref)
	slug := nonLabelChars.ReplaceAllString(lowered, "-")
	slug = strings.Trim(slug, "-")

	// The slug is faithful only when nothing had to be changed; otherwise two
	// refs could reach the same label and one preview would answer for both.
	exact := slug == ref

	suffix := "-" + refDigest(ref)
	if !exact && len(slug)+len(suffix) > MaxRefLabelLen {
		slug = slug[:MaxRefLabelLen-len(suffix)]
		slug = strings.TrimRight(slug, "-")
	} else if exact && len(slug) > MaxRefLabelLen {
		slug = slug[:MaxRefLabelLen-len(suffix)]
		slug = strings.TrimRight(slug, "-")
		exact = false
	}

	if exact {
		return slug
	}
	if slug == "" {
		// Nothing readable survived; the digest alone still names it uniquely.
		return "ref-" + refDigest(ref)
	}
	return slug + suffix
}

// refDigest is the short, stable discriminator appended to a shortened or
// scrubbed ref slug.
func refDigest(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return strings.ToLower(encoded[:refLabelHashLen])
}

// ResolveHost expands one host pattern against a ref and a domain. It is the
// single place a preview hostname is produced, so ingress and the CLI cannot
// disagree about what a preview is called.
func ResolveHost(pattern, ref, domain string) (string, error) {
	if pattern == "" {
		pattern = DefaultHostPattern
	}
	if domain == "" {
		return "", errors.New("no preview domain is configured")
	}

	host := strings.ReplaceAll(pattern, "{ref}", RefLabel(ref))
	host = strings.ReplaceAll(host, "{domain}", strings.Trim(domain, "."))
	host = strings.ToLower(strings.Trim(host, "."))

	if err := validateResolvedHost(host, domain); err != nil {
		return "", err
	}
	return host, nil
}

// validateResolvedHost checks that an expanded hostname is well-formed and sits
// inside the configured domain on a label boundary.
//
// The containment check is the security-relevant half. Without it a kvarn.yml
// on any branch could write `host: "admin.example.com"` and have the
// orchestrator serve that name — the file is authored by whoever opened the
// branch, and the domain belongs to the operator.
func validateResolvedHost(host, domain string) error {
	if host == "" {
		return errors.New("resolves to an empty hostname")
	}
	if len(host) > 253 {
		return fmt.Errorf("resolves to %q, which is longer than a hostname may be", host)
	}
	if !hostnameRe.MatchString(host) {
		return fmt.Errorf("resolves to %q, which is not a valid hostname", host)
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" {
			return fmt.Errorf("resolves to %q, which has an empty label", host)
		}
		if len(label) > MaxRefLabelLen {
			return fmt.Errorf("resolves to %q, whose label %q exceeds %d bytes", host, label, MaxRefLabelLen)
		}
	}

	base := strings.ToLower(strings.Trim(domain, "."))
	if host != base && !strings.HasSuffix(host, "."+base) {
		return fmt.Errorf("resolves to %q, which is outside the configured preview domain %q", host, base)
	}
	return nil
}

// ResolvedSite is one site of a preview with its hostname worked out.
type ResolvedSite struct {
	Name string
	Port uint16
	Host string
}

// URL is the address a browser reaches this site at. Previews are served over
// plain HTTP by kvarn and fronted by whatever terminates TLS, so the scheme is
// the operator's business; https is what a deployment that is reachable at all
// will be using.
func (s ResolvedSite) URL() string { return "https://" + s.Host }

// Resolve expands every site's host pattern for a ref, returning them sorted by
// site name so callers produce stable output.
func (p Preview) Resolve(ref, domain string) ([]ResolvedSite, error) {
	names := make([]string, 0, len(p.Sites))
	for name := range p.Sites {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]ResolvedSite, 0, len(names))
	for _, name := range names {
		site := p.Sites[name]
		host, err := ResolveHost(site.Host, ref, domain)
		if err != nil {
			return nil, fmt.Errorf("preview.sites.%s.host: %w", name, err)
		}
		out = append(out, ResolvedSite{Name: name, Port: site.Port, Host: host})
	}
	return out, nil
}

// EnvVarName is the environment variable a site's resolved URL is exported as
// before the serve commands run. Without it a server has no way to emit correct
// absolute URLs — for its own assets, for OAuth redirects — nor to tell its
// virtual hosts apart, and that is the most common way a preview environment
// ends up half-broken.
func EnvVarName(site string) string {
	return "KVARN_PREVIEW_URL_" + strings.ToUpper(strings.ReplaceAll(site, "-", "_"))
}

// validate checks the preview block on its own terms: names, ports, the shape
// of each host pattern, and that the serve steps and the sites agree on ports.
//
// The patterns are checked without a domain, since kvarn.yml is read long
// before the orchestrator's configured domain is in hand. What can be checked
// here is that a pattern names `{ref}` correctly and adds nothing that would
// take the result outside a domain suffix; the containment check itself
// happens at resolution.
func (p Preview) validate() error {
	if len(p.Sites) == 0 {
		if len(p.Serve) > 0 || len(p.Ready) > 0 {
			return errors.New("declares serve or ready steps but no sites")
		}
		return nil
	}

	names := make([]string, 0, len(p.Sites))
	for name := range p.Sites {
		names = append(names, name)
	}
	sort.Strings(names)

	// Hostnames are what route, so they are what must be unique. Ports need not
	// be: two sites on one port are two names on one virtual-hosting server,
	// and ingress hands it the hostname the browser asked for.
	usedHosts := make(map[string]string, len(p.Sites))
	sitePorts := make(map[uint16]string, len(p.Sites))
	for _, name := range names {
		site := p.Sites[name]
		if !previewSiteNameRe.MatchString(name) {
			return fmt.Errorf("site name %q must be lowercase alphanumerics separated by single hyphens", name)
		}
		if site.Port == 0 {
			return fmt.Errorf("site %q has no port", name)
		}
		sitePorts[site.Port] = name

		pattern := site.Host
		if pattern == "" {
			pattern = DefaultHostPattern
		}
		if other, dup := usedHosts[pattern]; dup {
			return fmt.Errorf("sites %q and %q both answer on host %q, so nothing could tell their requests apart",
				other, name, pattern)
		}
		usedHosts[pattern] = name

		if err := validatePreviewHostPattern(site.Host); err != nil {
			return fmt.Errorf("site %q host %q %s", name, site.Host, err)
		}
	}

	// Every port the sites name is bound by exactly one serve step, and every
	// serve step binds a port some site names: a port nothing starts is a
	// hostname that will never answer, a port no site names is a server nothing
	// can reach, and two steps on one port cannot both bind it.
	servedBy := make(map[uint16]string, len(p.Serve))
	for _, proc := range p.Serve {
		if strings.TrimSpace(proc.Name) == "" {
			return errors.New("serve step has empty name")
		}
		if strings.TrimSpace(proc.Run) == "" {
			return fmt.Errorf("serve step %q has empty run command", proc.Name)
		}
		if filepath.IsAbs(proc.WorkingDir) {
			return fmt.Errorf("serve step %q has absolute working_dir %q (must be relative)", proc.Name, proc.WorkingDir)
		}
		if proc.Port == 0 {
			return fmt.Errorf("serve step %q does not name a port", proc.Name)
		}
		if _, ok := sitePorts[proc.Port]; !ok {
			return fmt.Errorf("serve step %q serves port %d, which no site listens on", proc.Name, proc.Port)
		}
		if other, dup := servedBy[proc.Port]; dup {
			return fmt.Errorf("port %d is served by both %q and %q, but only one process can bind it",
				proc.Port, other, proc.Name)
		}
		servedBy[proc.Port] = proc.Name
		for _, name := range proc.Env {
			if !envNameRe.MatchString(name) {
				return fmt.Errorf("serve step %q env entry %q is not a valid POSIX env-var name", proc.Name, name)
			}
		}
	}
	for _, name := range names {
		if _, ok := servedBy[p.Sites[name].Port]; !ok {
			return fmt.Errorf("site %q has no serve step for port %d, so nothing would ever answer on its hostname",
				name, p.Sites[name].Port)
		}
	}

	for _, step := range p.Ready {
		if strings.TrimSpace(step.Name) == "" {
			return errors.New("ready check has empty name")
		}
		if strings.TrimSpace(step.Run) == "" {
			return fmt.Errorf("ready check %q has empty run command", step.Name)
		}
		if filepath.IsAbs(step.WorkingDir) {
			return fmt.Errorf("ready check %q has absolute working_dir %q (must be relative)", step.Name, step.WorkingDir)
		}
	}

	return nil
}

// validatePreviewHostPattern checks one site's host pattern before any domain is
// known. The returned string completes a sentence starting with the pattern.
func validatePreviewHostPattern(pattern string) error {
	if pattern == "" {
		// Absent means DefaultHostPattern, which is always well-formed.
		return nil
	}
	if strings.Contains(pattern, "://") {
		return errors.New("must not contain a scheme")
	}
	if strings.Contains(pattern, "/") {
		return errors.New("must not contain a path")
	}

	for _, match := range hostPatternPlaceholder.FindAllStringSubmatch(pattern, -1) {
		switch match[1] {
		case "ref", "domain":
		default:
			return fmt.Errorf("uses unknown placeholder %q; only {ref} and {domain} are available", match[0])
		}
	}

	// The pattern has to end in the domain, on a label boundary. Anything else
	// either names a fixed host in the operator's zone or a host outside it,
	// and both are the repository deciding what the operator's DNS means.
	if pattern != "{domain}" && !strings.HasSuffix(pattern, ".{domain}") {
		return errors.New("must end in {domain} or .{domain} so the name stays inside the configured preview domain")
	}

	// `{ref}` fills one label, so it must not be glued to a dot-free
	// neighbour that could push the label past its limit in a way the
	// repository controls — that part is fine — but it must not be split
	// across a dot, which would make it two labels.
	prefix := strings.TrimSuffix(pattern, "{domain}")
	prefix = strings.TrimSuffix(prefix, ".")
	if prefix != "" {
		for _, label := range strings.Split(prefix, ".") {
			if label == "" {
				return errors.New("has an empty label")
			}
			// Everything outside the placeholders has to be label-legal on its
			// own; the placeholder's own expansion is already one safe label.
			literal := strings.ReplaceAll(label, "{ref}", "r")
			if strings.Contains(literal, "{") || strings.Contains(literal, "}") {
				return errors.New("has an unclosed placeholder")
			}
			if !hostnameRe.MatchString(literal) {
				return fmt.Errorf("has label %q that is not valid in a hostname", label)
			}
		}
	}

	return nil
}
