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
// preview environment for this repository: which ports serve which hostnames,
// what to run to bring them up, and how to tell when they are ready.
//
// The operator owns the domain and the repository owns the shape. A host
// pattern here can only ever land inside the domain configured for the project,
// which is what keeps a branch from claiming a name in the operator's zone by
// editing this file.
type Preview struct {
	// Apps are the servers this preview exposes, keyed by the name the serve
	// steps refer to.
	Apps map[string]PreviewApp `yaml:"apps,omitempty"`
	// Serve are the long-lived commands that start the apps. Each names the app
	// it serves; every declared app must be served by exactly one.
	Serve []PreviewProcess `yaml:"serve,omitempty"`
	// Ready are the checks that decide when the preview may take traffic. They
	// are ordinary steps, run in order, and retried until they pass or the boot
	// gives up.
	Ready []Step `yaml:"ready,omitempty"`
}

// PreviewApp is one addressable server inside the preview.
type PreviewApp struct {
	// Port is the guest port the server listens on.
	Port uint16 `yaml:"port"`
	// Host is the hostname pattern this app answers on. It may use `{ref}` for
	// the slugged git ref and `{domain}` for the project's configured preview
	// domain. Empty defaults to DefaultHostPattern.
	Host string `yaml:"host,omitempty"`
}

// PreviewProcess is one long-lived command that serves an app.
type PreviewProcess struct {
	Name       string   `yaml:"name"`
	Run        string   `yaml:"run"`
	WorkingDir string   `yaml:"working_dir,omitempty"`
	App        string   `yaml:"app"`
	Env        []string `yaml:"env,omitempty"`
}

// DefaultHostPattern is what an app's `host` means when it is left out. It is
// the single-app answer: one preview, one name, derived from the ref.
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

// previewAppNameRe constrains an app name to something usable as an env-var
// suffix, since each app's URL is exported as KVARN_PREVIEW_URL_<APP>.
var previewAppNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// hostPatternPlaceholder matches the `{name}` placeholders a host pattern may
// contain, so an unknown one can be rejected rather than left in the hostname.
var hostPatternPlaceholder = regexp.MustCompile(`\{([a-z_]*)\}`)

// Enabled reports whether the repository declares a preview at all.
func (p Preview) Enabled() bool { return len(p.Apps) > 0 }

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

// ResolvedApp is one app of a preview with its hostname worked out.
type ResolvedApp struct {
	Name string
	Port uint16
	Host string
}

// URL is the address a browser reaches this app at. Previews are served over
// plain HTTP by kvarn and fronted by whatever terminates TLS, so the scheme is
// the operator's business; https is what a deployment that is reachable at all
// will be using.
func (a ResolvedApp) URL() string { return "https://" + a.Host }

// Resolve expands every app's host pattern for a ref, returning them sorted by
// app name so callers produce stable output.
func (p Preview) Resolve(ref, domain string) ([]ResolvedApp, error) {
	names := make([]string, 0, len(p.Apps))
	for name := range p.Apps {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]ResolvedApp, 0, len(names))
	for _, name := range names {
		app := p.Apps[name]
		host, err := ResolveHost(app.Host, ref, domain)
		if err != nil {
			return nil, fmt.Errorf("preview.apps.%s.host: %w", name, err)
		}
		out = append(out, ResolvedApp{Name: name, Port: app.Port, Host: host})
	}
	return out, nil
}

// EnvVarName is the environment variable an app's resolved URL is exported as
// before the serve commands run. Without it an app has no way to emit correct
// absolute URLs — for its own assets, for OAuth redirects — and that is the
// most common way a preview environment ends up half-broken.
func EnvVarName(app string) string {
	return "KVARN_PREVIEW_URL_" + strings.ToUpper(strings.ReplaceAll(app, "-", "_"))
}

// validate checks the preview block on its own terms: names, ports, the shape
// of each host pattern, and that serve and ready refer to apps that exist.
//
// The patterns are checked without a domain, since kvarn.yml is read long
// before the orchestrator's configured domain is in hand. What can be checked
// here is that a pattern names `{ref}` correctly and adds nothing that would
// take the result outside a domain suffix; the containment check itself
// happens at resolution.
func (p Preview) validate() error {
	if len(p.Apps) == 0 {
		if len(p.Serve) > 0 || len(p.Ready) > 0 {
			return errors.New("declares serve or ready steps but no apps")
		}
		return nil
	}

	names := make([]string, 0, len(p.Apps))
	for name := range p.Apps {
		names = append(names, name)
	}
	sort.Strings(names)

	usedPorts := make(map[uint16]string, len(p.Apps))
	for _, name := range names {
		app := p.Apps[name]
		if !previewAppNameRe.MatchString(name) {
			return fmt.Errorf("app name %q must be lowercase alphanumerics separated by single hyphens", name)
		}
		if app.Port == 0 {
			return fmt.Errorf("app %q has no port", name)
		}
		if other, dup := usedPorts[app.Port]; dup {
			return fmt.Errorf("apps %q and %q both listen on port %d", other, name, app.Port)
		}
		usedPorts[app.Port] = name

		if err := validatePreviewHostPattern(app.Host); err != nil {
			return fmt.Errorf("app %q host %q %s", name, app.Host, err)
		}
	}

	// Every app is served and every serve step names a real app: a declared
	// app nothing starts is a hostname that will never answer, and a serve step
	// for an app that does not exist is a server nothing can reach.
	servedBy := make(map[string]string, len(p.Serve))
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
		if proc.App == "" {
			return fmt.Errorf("serve step %q does not name an app", proc.Name)
		}
		if _, ok := p.Apps[proc.App]; !ok {
			return fmt.Errorf("serve step %q names unknown app %q", proc.Name, proc.App)
		}
		if other, dup := servedBy[proc.App]; dup {
			return fmt.Errorf("app %q is served by both %q and %q", proc.App, other, proc.Name)
		}
		servedBy[proc.App] = proc.Name
		for _, name := range proc.Env {
			if !envNameRe.MatchString(name) {
				return fmt.Errorf("serve step %q env entry %q is not a valid POSIX env-var name", proc.Name, name)
			}
		}
	}
	for _, name := range names {
		if _, ok := servedBy[name]; !ok {
			return fmt.Errorf("app %q has no serve step, so nothing would ever answer on its hostname", name)
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

// validatePreviewHostPattern checks one app's host pattern before any domain is
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
