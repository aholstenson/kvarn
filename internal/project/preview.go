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
// which ports, what to run to bring the preview up, and how to tell when it is
// ready.
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
	// Setup are one-shot commands run to completion, in order, before the serve
	// steps. They are where anything that has to know the preview's own
	// hostnames belongs — configuring domains, seeding a tenant, pointing a
	// running container at its URLs — since `setup.steps` runs long before a
	// hostname exists. A failure fails the boot.
	Setup []Step `yaml:"setup,omitempty"`
	// Serve are the long-lived commands that bring the preview up, started in
	// order and supervised for the preview's whole life. Which one binds which
	// port is the repository's business, and the ready checks are what decide
	// whether it worked. A repository whose servers are already running by the
	// end of setup — a container stack, say — declares none.
	Serve []PreviewProcess `yaml:"serve,omitempty"`
	// Ready are the checks that decide when the preview may take traffic. They
	// are ordinary steps, run in order, and retried until they pass or the boot
	// gives up.
	Ready []Step `yaml:"ready,omitempty"`
	// State is what survives the preview being stopped. Without it a preview
	// holds nothing: it is stopped when it goes idle, destroyed, and re-derived
	// from the branch on the next request, which loses whatever a reviewer
	// entered into it.
	State PreviewState `yaml:"state,omitempty"`
}

// PreviewState declares what a preview keeps between boots.
//
// Everything under GuestPreviewState is kept automatically — that directory is
// created for every preview and is what $KVARN_PREVIEW_STATE_DIR points at, so
// a compose stack that bind-mounts a volume out of it round-trips with nothing
// declared here at all. The fields exist for the two cases that need more: a
// data directory that lives somewhere else, and a database that would rather be
// kept as a logical dump than as raw files, which is also the honest answer to
// engine and schema drift between the commit that wrote the state and the one
// that reads it back.
type PreviewState struct {
	// Save runs before the state is captured, with the preview's servers still
	// up and the site URLs still in the environment. It is where a stack
	// quiesces a database or writes a dump into $KVARN_PREVIEW_STATE_DIR.
	Save []Step `yaml:"save,omitempty"`
	// Restore runs after the state has been unpacked and before the preview's
	// setup steps, and is the mirror of Save: loading the dump back.
	Restore []Step `yaml:"restore,omitempty"`
	// Paths are extra guest directories captured alongside the state directory,
	// for state that cannot be moved under it.
	Paths []string `yaml:"paths,omitempty"`
	// MaxSize caps the captured archive. A capture over the cap fails rather
	// than filling the operator's disk, and leaves the previous archive in
	// place. Empty means the operator's own ceiling is the only limit.
	MaxSize string `yaml:"max_size,omitempty"`
}

// Declared reports whether the repository has said anything about state. It is
// not the whole answer to "is there state to capture" — a preview that only
// ever wrote into $KVARN_PREVIEW_STATE_DIR declares nothing and still has data
// — but it is the half that can be answered without asking the guest.
func (s PreviewState) Declared() bool {
	return len(s.Save) > 0 || len(s.Restore) > 0 || len(s.Paths) > 0
}

// MaxSizeBytes returns the parsed cap, or 0 when none is declared.
func (s PreviewState) MaxSizeBytes() int64 {
	if s.MaxSize == "" {
		return 0
	}
	size, _ := ParseSize(s.MaxSize)
	return size
}

// PreviewSite is one hostname the preview answers on, and the port that answers
// it.
type PreviewSite struct {
	// Port is the guest port the server listens on. Several sites may name the
	// same port when one virtual-hosting server answers under several names;
	// what has to be unique is the hostname, since that is what routes.
	Port uint16 `yaml:"port"`
	// Host is the hostname pattern this site answers on. It may use `{ref}` for
	// the slugged git ref, `{pr}` for the pull request the preview belongs to,
	// and `{domain}` for the project's configured preview domain. Empty defaults
	// to DefaultHostPattern.
	//
	// A pattern using `{pr}` is what the operator's `auto_start` matches against
	// to turn a hostname nobody has visited yet into a preview, so a repository
	// that wants previews to start themselves names its sites by pull request
	// rather than by ref.
	Host string `yaml:"host,omitempty"`
}

// PreviewProcess is one long-lived command run to bring the preview up. Names
// are unique, since they are what identify the process in logs and events.
type PreviewProcess struct {
	Name       string   `yaml:"name"`
	Run        string   `yaml:"run"`
	WorkingDir string   `yaml:"working_dir,omitempty"`
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

// HostVars are the values a host pattern's placeholders expand to. A preview
// always knows its ref; it knows a pull request only when it was started for
// one, which is why PR is checked at expansion rather than assumed.
type HostVars struct {
	// Ref is the git ref the preview is pinned to, unslugged.
	Ref string
	// PR identifies the pull request the preview belongs to, in whatever
	// spelling the forge uses (a decimal number on GitHub). Empty when the
	// preview was started for a ref on its own.
	PR string
}

// prLabelRe constrains a pull request identifier to something that can stand as
// one DNS label. The value is opaque to kvarn — each forge spells its own — so
// it is checked rather than trusted: it ends up in a hostname the ingress
// routes on.
var prLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ResolveHost expands one host pattern against a preview's variables and a
// domain. It is the single place a preview hostname is produced, so ingress and
// the CLI cannot disagree about what a preview is called.
func ResolveHost(pattern string, vars HostVars, domain string) (string, error) {
	if pattern == "" {
		pattern = DefaultHostPattern
	}
	if domain == "" {
		return "", errors.New("no preview domain is configured")
	}

	if strings.Contains(pattern, "{pr}") {
		if vars.PR == "" {
			return "", errors.New("uses {pr}, but this preview was not started for a pull request")
		}
		if !prLabelRe.MatchString(strings.ToLower(vars.PR)) {
			return "", fmt.Errorf("uses {pr}, but %q cannot stand as a hostname label", vars.PR)
		}
	}

	host := strings.ReplaceAll(pattern, "{ref}", RefLabel(vars.Ref))
	host = strings.ReplaceAll(host, "{pr}", strings.ToLower(vars.PR))
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

// Resolve expands every site's host pattern for a preview, returning them
// sorted by site name so callers produce stable output.
func (p Preview) Resolve(vars HostVars, domain string) ([]ResolvedSite, error) {
	names := make([]string, 0, len(p.Sites))
	for name := range p.Sites {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]ResolvedSite, 0, len(names))
	for _, name := range names {
		site := p.Sites[name]
		host, err := ResolveHost(site.Host, vars, domain)
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

// EnvVarStateDir is the environment variable holding the directory a preview
// keeps state in. It is exported everywhere the site URLs are, so a save or
// restore hook, a setup step and a serve process all name the same place.
const EnvVarStateDir = "KVARN_PREVIEW_STATE_DIR"

// validate checks the preview block on its own terms: names, ports, the shape
// of each host pattern, and that every step it declares is runnable.
//
// The patterns are checked without a domain, since kvarn.yml is read long
// before the orchestrator's configured domain is in hand. What can be checked
// here is that a pattern names `{ref}` correctly and adds nothing that would
// take the result outside a domain suffix; the containment check itself
// happens at resolution.
func (p *Preview) validate(cachePaths []string) error {
	if len(p.Sites) == 0 {
		if len(p.Setup) > 0 || len(p.Serve) > 0 || len(p.Ready) > 0 || p.State.Declared() {
			return errors.New("declares setup, serve, ready or state but no sites")
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
	for _, name := range names {
		site := p.Sites[name]
		if !previewSiteNameRe.MatchString(name) {
			return fmt.Errorf("site name %q must be lowercase alphanumerics separated by single hyphens", name)
		}
		if site.Port == 0 {
			return fmt.Errorf("site %q has no port", name)
		}

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

	serveNames := make(map[string]struct{}, len(p.Serve))
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
		// Names identify a process in logs and events, so two steps sharing one
		// would make its output impossible to attribute.
		if _, dup := serveNames[proc.Name]; dup {
			return fmt.Errorf("serve step %q is declared twice", proc.Name)
		}
		serveNames[proc.Name] = struct{}{}
		for _, name := range proc.Env {
			if !envNameRe.MatchString(name) {
				return fmt.Errorf("serve step %q env entry %q is not a valid POSIX env-var name", proc.Name, name)
			}
		}
	}

	if err := validatePreviewSteps("setup step", p.Setup); err != nil {
		return err
	}
	if err := validatePreviewSteps("ready check", p.Ready); err != nil {
		return err
	}
	return p.State.validate(cachePaths)
}

// validate checks the state block and normalizes its paths in place, so
// everything downstream sees absolute guest paths.
func (s *PreviewState) validate(cachePaths []string) error {
	if err := validatePreviewSteps("state save step", s.Save); err != nil {
		return err
	}
	if err := validatePreviewSteps("state restore step", s.Restore); err != nil {
		return err
	}
	if s.MaxSize != "" {
		if _, err := ParseSize(s.MaxSize); err != nil {
			return fmt.Errorf("state.max_size: %w", err)
		}
	}

	seen := make(map[string]struct{}, len(s.Paths))
	for i, p := range s.Paths {
		norm, err := validateCachePath("state.paths", p)
		if err != nil {
			return err
		}
		if _, dup := seen[norm]; dup {
			return fmt.Errorf("state.paths entry %q is declared twice", p)
		}
		seen[norm] = struct{}{}

		// The state directory is captured whole already, so naming a path under
		// it puts the same bytes in the archive twice.
		if nestsUnder(norm, GuestPreviewState) {
			return fmt.Errorf(
				"state.paths entry %q is inside %s, which is already captured; drop the entry",
				p, GuestPreviewState)
		}
		// A directory that is both cached and kept as state has two mechanisms
		// writing it with different rules — the cache is write-once and
		// content-addressed, the state archive is last-write-wins — and which
		// one a boot ends up with would depend on ordering.
		for _, cached := range cachePaths {
			if nestsUnder(norm, cached) {
				return fmt.Errorf(
					"state.paths entry %q overlaps the cached path %q; a directory cannot be both cache and state",
					p, cached)
			}
		}
		s.Paths[i] = norm
	}
	return nil
}

// nestsUnder reports whether path is root or sits beneath it.
func nestsUnder(path, root string) bool {
	return path == root || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/")
}

// validatePreviewSteps checks the shape shared by the preview's one-shot step
// lists. The label completes a sentence naming the offending entry.
func validatePreviewSteps(label string, steps []Step) error {
	for _, step := range steps {
		if strings.TrimSpace(step.Name) == "" {
			return fmt.Errorf("%s has empty name", label)
		}
		if strings.TrimSpace(step.Run) == "" {
			return fmt.Errorf("%s %q has empty run command", label, step.Name)
		}
		if filepath.IsAbs(step.WorkingDir) {
			return fmt.Errorf("%s %q has absolute working_dir %q (must be relative)", label, step.Name, step.WorkingDir)
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
		case "ref", "pr", "domain":
		default:
			return fmt.Errorf("uses unknown placeholder %q; only {ref}, {pr} and {domain} are available", match[0])
		}
	}

	// The pattern has to end in the domain, on a label boundary. Anything else
	// either names a fixed host in the operator's zone or a host outside it,
	// and both are the repository deciding what the operator's DNS means.
	if pattern != "{domain}" && !strings.HasSuffix(pattern, ".{domain}") {
		return errors.New("must end in {domain} or .{domain} so the name stays inside the configured preview domain")
	}

	// `{ref}` and `{pr}` each fill part of one label, so neither may be split
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
			literal = strings.ReplaceAll(literal, "{pr}", "1")
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
