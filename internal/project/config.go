package project

import (
	"math"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Guest paths inside the VM. The runner runs jobs as the unprivileged "kvarn"
// user, with the project source mounted at GuestWorkspace.
const (
	GuestHome      = "/home/kvarn"
	GuestWorkspace = "/home/kvarn/workspace"
	// GuestPreviewState is the directory a preview environment keeps state in
	// between boots. It sits outside GuestWorkspace on purpose: the workspace is
	// a fresh clone on every boot, so anything kept there would be clobbered by
	// the next one.
	GuestPreviewState = "/home/kvarn/state"
)

const (
	// DefaultDiskSize is the default VM disk size (16 GiB).
	DefaultDiskSize int64 = 16 * 1024 * 1024 * 1024
	// MinDiskSize is the minimum allowed VM disk size (4 GiB).
	MinDiskSize int64 = 4 * 1024 * 1024 * 1024

	// DefaultCPUs is the default number of vCPUs.
	DefaultCPUs uint = 2
	// MinCPUs is the minimum allowed vCPU count.
	MinCPUs uint = 1

	// DefaultMemory is the default VM memory size (4 GiB).
	DefaultMemory uint64 = 4 * 1024 * 1024 * 1024
	// MinMemory is the minimum allowed VM memory size (2 GiB).
	MinMemory uint64 = 2 * 1024 * 1024 * 1024

	// NixpkgsFlakePrefix is the flake URI prefix every `nixpkgs` source
	// resolves under. Code that has to recognise a resolved dependency as
	// coming from nixpkgs (tool curation, cache keying) matches on this
	// instead of repeating the literal.
	NixpkgsFlakePrefix = "github:NixOS/nixpkgs/"

	// DefaultNixpkgsChannel is the nixpkgs release a bare `nixpkgs` source
	// tracks. It is the name the reference docs promise users, and a test in
	// this package fails if docs/reference/kvarn-yml.md still names the
	// previous one.
	DefaultNixpkgsChannel = "nixos-26.05"

	// DefaultNixpkgsRev is a known-good commit on DefaultNixpkgsChannel, and
	// the floor under every other way of arriving at one.
	//
	// A dependency install has to start from a commit: handed a branch name,
	// Nix resolves it from inside the VM with a call to api.github.com that no
	// Nix cache stands in for and nothing catches when it fails. kvarn
	// therefore resolves the channel on the host (internal/nixpkgs), which is
	// what a job normally installs from — this constant is what that resolution
	// falls back to, and what a build with no network at boot uses.
	//
	// Refresh it when moving the channel, so the fallback is not years old:
	//
	//	git ls-remote https://github.com/NixOS/nixpkgs refs/heads/<channel>
	DefaultNixpkgsRev = "b18a4b905f8d028dc4476412e6d6891728695379"

	// DefaultNixpkgsFlake is the flake URI a bare `nixpkgs` source resolves to
	// before the host resolves its channel to a current commit.
	DefaultNixpkgsFlake = NixpkgsFlakePrefix + DefaultNixpkgsRev
)

// Config represents a project-level configuration file (kvarn.yml).
type Config struct {
	Dependencies Dependencies      `yaml:"dependencies,omitempty"`
	VM           VM                `yaml:"vm"`
	Network      Network           `yaml:"network"`
	Cache        Cache             `yaml:"cache"`
	Environment  map[string]string `yaml:"environment,omitempty"`
	Secrets      []SecretRef       `yaml:"secrets,omitempty"`
	Setup        Setup             `yaml:"setup"`
	Validation   Validation        `yaml:"validation"`
	Modes        Modes             `yaml:"modes,omitempty"`
	PullRequest  PullRequest       `yaml:"pull_request,omitempty"`
	Preview      Preview           `yaml:"preview,omitempty"`
}

// Modes are the agent modes this repository defines, keyed by the name a job
// selects with `--mode`. They sit beside the built-in modes rather than
// replacing them: a definition inherits from a built-in (or from another mode
// in the same file) and overrides only the axes it names.
type Modes map[string]ModeSpec

// ModeSpec is one entry in the `modes:` map. Every field is optional; an unset
// axis takes its value from the mode named by `extends`, or from that axis's
// own default.
//
// The vocabulary is deliberately the same one the orchestrator resolves against
// (see internal/agent/coding). It is restated here as plain strings because the
// coding package sits downstream of this one, and validation below is what
// keeps a typo in a kvarn.yml from reaching the run as a mode that quietly does
// the wrong thing.
type ModeSpec struct {
	// Description is a one-line summary shown by `kvarn modes list`.
	Description string `yaml:"description,omitempty"`
	// Extends names the mode this one inherits from. Empty means `auto`.
	Extends string `yaml:"extends,omitempty"`
	// Prompt is appended to the inherited prompt; it adds instructions rather
	// than replacing them.
	Prompt string `yaml:"prompt,omitempty"`
	// Workspace is "read-only" or "read-write".
	Workspace string `yaml:"workspace,omitempty"`
	// Validation is "skip", "run", or "require".
	Validation string `yaml:"validation,omitempty"`
	// Deliver lists where the result goes: "none", "pr-comment",
	// "follow-up-commit", "new-pull-request".
	Deliver []string `yaml:"deliver,omitempty"`
	// Start constrains where a run may begin: "branch", "pull-request", "any".
	Start string `yaml:"start,omitempty"`
	// Context lists the sections prepended to the task message:
	// "original-task", "pr-metadata", "pr-diff".
	Context []string `yaml:"context,omitempty"`
}

// Cache defines additional guest-side paths to persist across VM runs.
// Registered tools are cached automatically and need no cache: block; these
// fields are power-user overrides for unregistered tools or custom keying.
type Cache struct {
	Paths   []string     `yaml:"paths,omitempty"`   // unkeyed guest paths
	Entries []CacheEntry `yaml:"entries,omitempty"` // keyed cache entries
}

// CacheEntry is a power-user cache override for a single guest path.
//
//   - Key set: a fully manual, fixed cache key (the caller owns invalidation).
//   - Lockfiles set: content-addressed like a registered tool.
//   - Neither set: an unkeyed (write-once) cache for the path.
type CacheEntry struct {
	Path      string   `yaml:"path"`
	Lockfiles []string `yaml:"lockfiles,omitempty"`
	Key       string   `yaml:"key,omitempty"`
	Bucket    string   `yaml:"bucket,omitempty"`
}

// Network defines network egress controls for the VM.
type Network struct {
	AllowedHosts []string `yaml:"allowed_hosts,omitempty"`

	// HostAliases maps a hostname to the IP address it resolves to inside the
	// VM. Its purpose is local development names — a dev server on 127.0.0.1
	// reachable as dev-shop.example.local.
	//
	// A key is either one literal name or the "*.domain" wildcard form, which
	// matches any subdomain of that suffix. The two are answered by different
	// machinery in the guest (see ExactHostAliases), but they are one key here
	// because they are one idea to whoever writes the file.
	HostAliases map[string]string `yaml:"host_aliases,omitempty"`
}

// ExactHostAliases returns the entries naming one literal host, which are the
// only ones expressible as /etc/hosts lines. Wildcards are left out: that file
// matches exactly and has no syntax for a suffix, so those entries are served
// by kvarn's DNS forwarder instead. Every entry, wildcard or not, reaches the
// forwarder, so the two never disagree about a name they both cover.
func (n Network) ExactHostAliases() map[string]string {
	if len(n.HostAliases) == 0 {
		return nil
	}
	exact := make(map[string]string, len(n.HostAliases))
	for name, addr := range n.HostAliases {
		if !strings.HasPrefix(name, "*.") {
			exact[name] = addr
		}
	}
	if len(exact) == 0 {
		return nil
	}
	return exact
}

// SecretRef is a single entry in the kvarn.yml `secrets:` list. It declares a
// secret the project needs and, for managed secrets, how and where the egress
// proxy should apply it. Both scheme and hosts are usage-site concerns: the
// store type (env vs managed) only governs whether the real value enters the
// VM, while scheme/hosts describe the protocol and scope at the call site.
//
// An entry may be written as a bare scalar (`- NAME`), which is shorthand for
// the default scheme over any allowlisted host, or as a mapping with explicit
// `name`, `scheme`, and `hosts` fields.
type SecretRef struct {
	// Name is the secret's name; it is exposed inside the VM as this env var.
	Name string `yaml:"name"`
	// Scheme selects how a managed secret is applied to an outbound request.
	// Empty defaults to "bearer" at resolution time. One of "", "bearer",
	// "basic", "oauth".
	Scheme string `yaml:"scheme,omitempty"`
	// Hosts scopes substitution to a set of allowlist host patterns (same
	// wildcard syntax as network.allowed_hosts). Empty means any allowlisted
	// host.
	Hosts []string `yaml:"hosts,omitempty"`
}

// UnmarshalYAML accepts either a scalar (a bare secret name) or a mapping with
// name/scheme/hosts fields.
func (r *SecretRef) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		return value.Decode(&r.Name)
	}
	// Use an alias type to avoid recursing into this method.
	type rawRef SecretRef
	var raw rawRef
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*r = SecretRef(raw)
	return nil
}

// hostnameRe validates hostnames per RFC 952/1123.
var hostnameRe = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?\.)*[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?$`)

// nixpkgsChannelRe validates the channel suffix in `nixpkgs/<channel>`.
var nixpkgsChannelRe = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// nixAttrRe validates a flake attribute name. Conservative on purpose: we
// concatenate the attr into a shell command, so only safe identifiers pass.
var nixAttrRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9._-]*$`)

// envNameRe validates POSIX-style env-var names.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Dependencies maps a flake reference to the attribute names to install
// from that flake. Iteration order is not preserved.
//
// Source resolution:
//
//	"nixpkgs"           → github:NixOS/nixpkgs/<DefaultNixpkgsRev>
//	"nixpkgs/<channel>" → github:NixOS/nixpkgs/<channel>
//	anything else       → flake URI verbatim
type Dependencies map[string][]string

// ResolvedDep is a single attribute-from-flake pair after schema resolution.
type ResolvedDep struct {
	FlakeURI string // canonical flake reference
	Attr     string // attribute path to install
	Host     string // hostname for firewall allowlist (may be empty)

	// Channel is the nixpkgs channel this dependency tracks, and is empty for
	// every other kind of source. It exists because FlakeURI may name the
	// commit the channel currently points at, and that commit moves: the
	// channel name is the part of a dependency's identity that does not.
	Channel string
}

// StableURI is the flake reference to key caches by: the channel a nixpkgs
// dependency tracks rather than whichever commit it resolves to today. Two
// runs on either side of a channel advance name the same thing here, which is
// what keeps a project's tool caches warm across a nixpkgs that has merely
// moved.
func (d ResolvedDep) StableURI() string {
	if d.Channel != "" {
		return NixpkgsFlakePrefix + d.Channel
	}
	return d.FlakeURI
}

// flakeRef is one resolved dependency source, before attributes are attached.
type flakeRef struct {
	URI     string // canonical flake reference
	Host    string // hostname needing firewall egress
	Channel string // nixpkgs channel, empty for other sources
}

// Resolve expands the source map into a flat slice of ResolvedDep entries.
// Each source is resolved to a canonical flake URI; each attribute is
// validated against nixAttrRe.
func (d Dependencies) Resolve() ([]ResolvedDep, error) {
	var out []ResolvedDep
	for source, attrs := range d {
		ref, err := resolveFlakeRef(source)
		if err != nil {
			return nil, err
		}
		if len(attrs) == 0 {
			return nil, fmt.Errorf("dependency source %q has no attributes", source)
		}
		for _, attr := range attrs {
			if !nixAttrRe.MatchString(attr) {
				return nil, fmt.Errorf("invalid attribute %q for source %q: must match %s",
					attr, source, nixAttrRe.String())
			}
			out = append(out, ResolvedDep{
				FlakeURI: ref.URI,
				Attr:     attr,
				Host:     ref.Host,
				Channel:  ref.Channel,
			})
		}
	}
	return out, nil
}

// resolveFlakeRef converts a user-facing source string into a canonical flake
// URI plus the hostname that needs firewall egress. Unknown forms are
// rejected with a friendly error.
func resolveFlakeRef(source string) (flakeRef, error) {
	s := strings.TrimSpace(source)
	if s == "" {
		return flakeRef{}, errors.New("dependency source must not be empty")
	}

	switch {
	case s == "nixpkgs":
		return flakeRef{URI: DefaultNixpkgsFlake, Host: "github.com", Channel: DefaultNixpkgsChannel}, nil

	case strings.HasPrefix(s, "nixpkgs/"):
		channel := strings.TrimPrefix(s, "nixpkgs/")
		if channel == "" {
			return flakeRef{}, fmt.Errorf("invalid nixpkgs source %q: channel must not be empty", source)
		}
		if !nixpkgsChannelRe.MatchString(channel) {
			return flakeRef{}, fmt.Errorf("invalid nixpkgs channel %q: must match %s",
				channel, nixpkgsChannelRe.String())
		}
		return flakeRef{URI: NixpkgsFlakePrefix + channel, Host: "github.com", Channel: channel}, nil

	case strings.HasPrefix(s, "github:"):
		return flakeRef{URI: s, Host: "github.com"}, nil

	case strings.HasPrefix(s, "gitlab:"):
		return flakeRef{URI: s, Host: "gitlab.com"}, nil

	case strings.HasPrefix(s, "git+https://"),
		strings.HasPrefix(s, "git+ssh://"),
		strings.HasPrefix(s, "https://"),
		strings.HasPrefix(s, "http://"),
		strings.HasPrefix(s, "tarball+http://"),
		strings.HasPrefix(s, "tarball+https://"):
		// Strip the "git+"/"tarball+" prefix so net/url can parse the URL.
		raw := s
		raw = strings.TrimPrefix(raw, "git+")
		raw = strings.TrimPrefix(raw, "tarball+")
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			return flakeRef{}, fmt.Errorf("invalid dependency source %q: %v", source, err)
		}
		return flakeRef{URI: s, Host: u.Hostname()}, nil
	}

	return flakeRef{}, fmt.Errorf("unsupported dependency source %q: expected `nixpkgs`, "+
		"`nixpkgs/<channel>`, `github:owner/repo[/ref]`, `gitlab:owner/repo[/ref]`, "+
		"`git+https://...`, `git+ssh://...`, `https://...`, or `tarball+https://...`", source)
}

// VM defines VM-level configuration overrides.
type VM struct {
	Disk   string `yaml:"disk,omitempty"`   // e.g. "4G", "8G", "16G"
	CPUs   uint   `yaml:"cpus,omitempty"`   // e.g. 2, 4
	Memory string `yaml:"memory,omitempty"` // e.g. "2G", "4G", "512M"
}

// DiskSizeBytes returns the parsed disk size in bytes, or 0 if not set.
func (c *Config) DiskSizeBytes() int64 {
	if c.VM.Disk == "" {
		return 0
	}
	size, _ := ParseSize(c.VM.Disk)
	return size
}

// CPUs returns the configured vCPU count, or 0 if not set.
func (c *Config) CPUs() uint {
	return c.VM.CPUs
}

// MemoryBytes returns the parsed memory size in bytes, or 0 if not set.
func (c *Config) MemoryBytes() uint64 {
	if c.VM.Memory == "" {
		return 0
	}
	size, _ := ParseSize(c.VM.Memory)
	return uint64(size)
}

// ParseSize parses a human-readable size string into bytes.
// Supports suffixes: M, MiB (mebibytes), G, GiB (gibibytes).
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty size")
	}

	var suffix string
	var numStr string
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] >= '0' && s[i] <= '9' || s[i] == '.' {
			numStr = s[:i+1]
			suffix = strings.TrimSpace(s[i+1:])
			break
		}
	}

	if numStr == "" {
		return 0, fmt.Errorf("invalid size %q: no numeric value", s)
	}

	// Parse as integer (no fractional sizes).
	var value int64
	for _, c := range numStr {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid size %q: non-integer value", s)
		}
		value = value*10 + int64(c-'0')
	}

	switch strings.ToUpper(suffix) {
	case "M", "MIB":
		return value * 1024 * 1024, nil
	case "G", "GIB":
		return value * 1024 * 1024 * 1024, nil
	default:
		return 0, fmt.Errorf("invalid size %q: unsupported suffix %q (use M, MiB, G, or GiB)", s, suffix)
	}
}

// Setup defines steps to run before the agent and health checks to verify readiness.
type Setup struct {
	Steps        []Step `yaml:"steps"`
	HealthChecks []Step `yaml:"health_checks"`
}

// Validation defines required and advisory validation steps to run after the agent.
type Validation struct {
	Required []Step `yaml:"required"`
	Advisory []Step `yaml:"advisory"`
}

// Step represents a single command to execute.
type Step struct {
	Name       string   `yaml:"name"`
	Run        string   `yaml:"run"`
	WorkingDir string   `yaml:"working_dir,omitempty"`
	Paths      []string `yaml:"paths,omitempty"`
	Timeout    Duration `yaml:"timeout,omitempty"`
	// Retry specifies how many additional attempts to make if the step fails.
	// 0 (the default) means no retries. Only honoured for setup steps.
	Retry uint `yaml:"retry,omitempty"`
}

// Duration is a YAML-friendly duration that accepts either a plain number
// (interpreted as seconds) or a Go duration string (e.g. "10m", "1h30m").
type Duration time.Duration

// Seconds returns the duration rounded to whole seconds.
func (d Duration) Seconds() uint32 {
	s := time.Duration(d).Seconds()
	if s > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(s)
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	// Try plain integer first (seconds).
	var secs uint64
	if err := value.Decode(&secs); err == nil {
		*d = Duration(time.Duration(secs) * time.Second)
		return nil
	}

	// Try duration string (e.g. "10m", "1h30m", "30s").
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("timeout must be a number (seconds) or duration string (e.g. \"10m\")")
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid timeout %q: must be a number (seconds) or duration string (e.g. \"10m\")", s)
	}
	if parsed < 0 {
		return fmt.Errorf("timeout must not be negative")
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (any, error) {
	dur := time.Duration(d)
	if dur == 0 {
		return 0, nil
	}
	return dur.String(), nil
}

// configFileNames lists the config file names in priority order.
var configFileNames = []string{
	"kvarn.yml",
	"kvarn.yaml",
	".kvarn.yml",
	".kvarn.yaml",
}

// Load reads and parses the project config from the given directory.
// It searches for config files in priority order: kvarn.yml > kvarn.yaml > .kvarn.yml > .kvarn.yaml.
// Returns nil, nil if no config file exists.
func Load(dir string) (*Config, error) {
	for _, name := range configFileNames {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", name, err)
		}

		// yaml.v3 silently drops unknown fields on the typed unmarshal; sniff
		// for keys that used to mean something so users get a clear migration
		// error rather than a VM that quietly lacks the environment they asked
		// for.
		var raw map[string]yaml.Node
		if unmarshalErr := yaml.Unmarshal(data, &raw); unmarshalErr == nil {
			if _, ok := raw["tools"]; ok {
				return nil, fmt.Errorf("`tools:` has been replaced by `dependencies:` in %s; "+
					"see https://github.com/aholstenson/kvarn for migration", name)
			}
			if _, ok := raw["image"]; ok {
				return nil, fmt.Errorf("`image:` is no longer supported in %s; "+
					"declare the toolchain with `dependencies:` instead", name)
			}
		}

		var cfg Config
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}

		if err := cfg.validate(); err != nil {
			return nil, fmt.Errorf("validate %s: %w", name, err)
		}

		return &cfg, nil
	}

	return nil, nil
}

// normalizeCachePath resolves a user-supplied cache path into an absolute
// guest path:
//   - "~" and "~/foo" expand against GuestHome
//   - relative paths resolve under GuestWorkspace (and must not escape it via "..")
//   - absolute paths are cleaned but otherwise left alone
func normalizeCachePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("path is empty")
	}
	switch {
	case p == "~":
		return GuestHome, nil
	case strings.HasPrefix(p, "~/"):
		return path.Join(GuestHome, p[2:]), nil
	case path.IsAbs(p):
		return path.Clean(p), nil
	default:
		abs := path.Join(GuestWorkspace, p)
		if abs != GuestWorkspace && !strings.HasPrefix(abs, GuestWorkspace+"/") {
			return "", fmt.Errorf("relative path %q escapes the workspace", p)
		}
		return abs, nil
	}
}

// validateCachePath normalizes a user-supplied cache path and enforces that it
// resolves to a usable absolute guest path: not the workspace root itself
// (which is transferred separately), and not under /nix (the store cannot
// round-trip as a plain tarball; a Nix cache is a first-class, separate
// mechanism). Subpaths of the workspace are permitted.
func validateCachePath(field, original string) (string, error) {
	norm, err := normalizeCachePath(original)
	if err != nil {
		return "", fmt.Errorf("%s entry %q: %v", field, original, err)
	}
	if norm == GuestWorkspace {
		return "", fmt.Errorf("%s entry %q resolves to the workspace root, which is transferred separately", field, original)
	}
	if norm == "/nix" || strings.HasPrefix(norm, "/nix/") {
		return "", fmt.Errorf("%s entry %q is not allowed; caching /nix is a first-class feature", field, original)
	}
	return norm, nil
}

// validateHostPattern validates a single host entry from an allowlist (either
// network.allowed_hosts or a secret's scoping `hosts:`). It accepts hostnames,
// IP addresses, and the "*.domain" wildcard form, and rejects schemes, paths,
// and ports. field is used for error context.
func validateHostPattern(field, host string) error {
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("%s contains empty entry", field)
	}
	if strings.Contains(host, "://") {
		return fmt.Errorf("%s entry %q must not contain a scheme", field, host)
	}
	if strings.Contains(host, "/") {
		return fmt.Errorf("%s entry %q must not contain a path", field, host)
	}
	// A "*.example.com" wildcard matches any subdomain; validate the suffix.
	check := strings.TrimPrefix(host, "*.")
	// Check for port, but skip IPv6 addresses (which contain colons).
	if net.ParseIP(check) == nil && strings.Contains(check, ":") {
		return fmt.Errorf("%s entry %q must not contain a port", field, host)
	}
	if net.ParseIP(check) == nil && !hostnameRe.MatchString(check) {
		return fmt.Errorf("%s entry %q is not a valid hostname or IP", field, host)
	}
	return nil
}

// validateHostName validates a single name being mapped to an address. It
// accepts one literal hostname or the "*.domain" wildcard form. Unlike
// validateHostPattern it rejects a bare IP address, which names nothing and
// would silently map an address to itself. field is used for error context.
func validateHostName(field, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%s contains an empty hostname", field)
	}
	if strings.Contains(name, "://") {
		return fmt.Errorf("%s entry %q must not contain a scheme", field, name)
	}
	if strings.ContainsAny(name, "/:") {
		return fmt.Errorf("%s entry %q must not contain a path or port", field, name)
	}
	// Only a leading "*." is a wildcard; a star anywhere else is a name the
	// guest could never be asked to resolve.
	check := strings.TrimPrefix(name, "*.")
	if strings.Contains(check, "*") {
		return fmt.Errorf("%s entry %q may only use a wildcard as a leading \"*.\" label", field, name)
	}
	if net.ParseIP(check) != nil {
		return fmt.Errorf("%s entry %q is an IP address, not a hostname", field, name)
	}
	if !hostnameRe.MatchString(check) {
		return fmt.Errorf("%s entry %q is not a valid hostname", field, name)
	}
	return nil
}

// secretSchemes is the set of accepted kvarn.yml secret schemes. The empty
// string is accepted and defaults to bearer at resolution time.
var secretSchemes = map[string]bool{"": true, "bearer": true, "basic": true, "oauth": true}

// The accepted values for each `modes:` axis. The empty string is accepted
// everywhere and means "inherit"; see ModeSpec.
var (
	modeWorkspaces  = map[string]bool{"": true, "read-only": true, "read-write": true}
	modeValidations = map[string]bool{"": true, "skip": true, "run": true, "require": true}
	modeStarts      = map[string]bool{"": true, "branch": true, "pull-request": true, "any": true}
	modeSinks       = map[string]bool{"none": true, "pr-comment": true, "follow-up-commit": true, "new-pull-request": true}
	modeContexts    = map[string]bool{"none": true, "original-task": true, "pr-metadata": true, "pr-diff": true}
)

// builtinModeNames are the modes kvarn ships with. A repository may extend one
// but not redefine it, so the names are reserved.
//
// It mirrors coding.Builtins(), which this package cannot read: the coding
// package depends on this one through the sandbox. A test asserts the two lists
// agree, so a mode added there is caught here rather than drifting.
var builtinModeNames = []string{"auto", "implement", "fix", "feedback", "review", "research"}

// BuiltinModeNames returns the reserved mode names, for callers that need the
// same list this package validates against.
func BuiltinModeNames() []string { return append([]string(nil), builtinModeNames...) }

// modeNameRe constrains a mode name to lowercase alphanumerics separated by
// single hyphens, matching what the orchestrator accepts on `--mode`.
var modeNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// maxModeNameLen bounds a mode name so an unbounded string cannot travel from a
// kvarn.yml into every log line a run produces.
const maxModeNameLen = 64

// Resolve checks every mode definition and how they relate: names are
// well-formed and do not shadow a built-in, each axis holds a value from its
// vocabulary, the combination is coherent, and `extends` reaches a real mode
// without going in a circle. It returns the definitions in dependency order —
// every mode after the one it extends — which is the order they can be built
// in.
//
// It has no effect of its own; validate() calls it so a bad definition is
// reported when the file is read rather than when a job selects the mode.
func (m Modes) Resolve() ([]string, error) {
	if len(m) == 0 {
		return nil, nil
	}

	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)

	reserved := make(map[string]bool, len(builtinModeNames))
	for _, name := range builtinModeNames {
		reserved[name] = true
	}

	for _, name := range names {
		if err := validateModeName(name); err != nil {
			return nil, err
		}
		if reserved[name] {
			return nil, fmt.Errorf("mode %q is built in and cannot be redefined", name)
		}
		if err := m[name].validate(name); err != nil {
			return nil, err
		}
		if parent := m[name].Extends; parent != "" && !reserved[parent] {
			if _, ok := m[parent]; !ok {
				return nil, fmt.Errorf("mode %q extends unknown mode %q", name, parent)
			}
		}
	}

	// Depth-first over `extends`, emitting each mode after its parent. The
	// visiting set is what catches a cycle: reaching a mode that is still on
	// the stack means following extends leads back to where it started.
	var order []string
	done := make(map[string]bool, len(names))
	visiting := make(map[string]bool, len(names))
	var walk func(name string) error
	walk = func(name string) error {
		if done[name] || reserved[name] {
			return nil
		}
		if visiting[name] {
			return fmt.Errorf("mode %q extends itself through a cycle", name)
		}
		visiting[name] = true
		if parent := m[name].Extends; parent != "" {
			if err := walk(parent); err != nil {
				return err
			}
		}
		delete(visiting, name)
		done[name] = true
		order = append(order, name)
		return nil
	}
	for _, name := range names {
		if err := walk(name); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// validateModeName enforces the shape of a mode name.
func validateModeName(name string) error {
	switch {
	case name == "":
		return errors.New("modes contains an empty name")
	case len(name) > maxModeNameLen:
		return fmt.Errorf("mode name %q is %d bytes; the limit is %d", name, len(name), maxModeNameLen)
	case !modeNameRe.MatchString(name):
		return fmt.Errorf("mode name %q must be lowercase alphanumerics separated by single hyphens", name)
	}
	return nil
}

// validate checks one definition's axes in the order they are documented.
func (s ModeSpec) validate(name string) error {
	if s.Extends != "" {
		if err := validateModeName(s.Extends); err != nil {
			return fmt.Errorf("mode %q extends: %w", name, err)
		}
	}
	if !modeWorkspaces[s.Workspace] {
		return fmt.Errorf("mode %q has invalid workspace %q: must be read-only or read-write", name, s.Workspace)
	}
	if !modeValidations[s.Validation] {
		return fmt.Errorf("mode %q has invalid validation %q: must be skip, run or require", name, s.Validation)
	}
	if !modeStarts[s.Start] {
		return fmt.Errorf("mode %q has invalid start %q: must be branch, pull-request or any", name, s.Start)
	}

	// An empty list is refused rather than read as either intent. Omitting the
	// key inherits the base mode's sinks, and `[none]` delivers nothing; a
	// written-out `[]` reads like the second but would do the first, so it is
	// answered with the spelling that means what it looks like.
	if s.Deliver != nil && len(s.Deliver) == 0 {
		return fmt.Errorf("mode %q has an empty deliver list: write deliver: [none] to deliver nothing, or omit deliver to inherit", name)
	}
	if s.Context != nil && len(s.Context) == 0 {
		return fmt.Errorf("mode %q has an empty context list: write context: [none] to assemble no context, or omit context to inherit", name)
	}

	seenSink := make(map[string]bool, len(s.Deliver))
	for _, sink := range s.Deliver {
		if !modeSinks[sink] {
			return fmt.Errorf("mode %q has invalid deliver %q: must be one of none, pr-comment, follow-up-commit, new-pull-request", name, sink)
		}
		if seenSink[sink] {
			return fmt.Errorf("mode %q lists deliver %q twice", name, sink)
		}
		seenSink[sink] = true
	}
	if seenSink["none"] && len(s.Deliver) > 1 {
		return fmt.Errorf("mode %q combines deliver none with another sink", name)
	}
	if seenSink["follow-up-commit"] && seenSink["new-pull-request"] {
		return fmt.Errorf("mode %q delivers both follow-up-commit and new-pull-request: changes land in one place or the other", name)
	}
	if s.Workspace == "read-only" && (seenSink["follow-up-commit"] || seenSink["new-pull-request"]) {
		return fmt.Errorf("mode %q is read-only, so it has no changes to deliver as a commit", name)
	}
	if s.Start == "branch" && seenSink["follow-up-commit"] {
		return fmt.Errorf("mode %q delivers follow-up-commit but can only start from a branch, which has no pull request to commit onto", name)
	}

	seenBlock := make(map[string]bool, len(s.Context))
	for _, block := range s.Context {
		if !modeContexts[block] {
			return fmt.Errorf("mode %q has invalid context %q: must be one of none, original-task, pr-metadata, pr-diff", name, block)
		}
		if seenBlock[block] {
			return fmt.Errorf("mode %q lists context %q twice", name, block)
		}
		seenBlock[block] = true
	}
	if seenBlock["none"] && len(s.Context) > 1 {
		return fmt.Errorf("mode %q combines context none with another block", name)
	}

	return nil
}

func (c *Config) validate() error {
	// Surface dependency schema errors at load time.
	if len(c.Dependencies) > 0 {
		if _, err := c.Dependencies.Resolve(); err != nil {
			return fmt.Errorf("dependencies: %w", err)
		}
	}

	if c.VM.Disk != "" {
		size, err := ParseSize(c.VM.Disk)
		if err != nil {
			return fmt.Errorf("vm.disk: %w", err)
		}
		if size < MinDiskSize {
			return fmt.Errorf("vm.disk %q is below minimum of 4G", c.VM.Disk)
		}
	}

	if c.VM.CPUs != 0 && c.VM.CPUs < MinCPUs {
		return fmt.Errorf("vm.cpus %d is below minimum of %d", c.VM.CPUs, MinCPUs)
	}

	if c.VM.Memory != "" {
		size, err := ParseSize(c.VM.Memory)
		if err != nil {
			return fmt.Errorf("vm.memory: %w", err)
		}
		if uint64(size) < MinMemory {
			return fmt.Errorf("vm.memory %q is below minimum of 2G", c.VM.Memory)
		}
	}

	// Validate network allowed_hosts.
	for _, host := range c.Network.AllowedHosts {
		if err := validateHostPattern("network.allowed_hosts", host); err != nil {
			return err
		}
	}

	// Validate network host_aliases. The value is stricter than an allowlist
	// entry: it is the literal address the name resolves to, so a hostname
	// there would resolve to nothing.
	for name, addr := range c.Network.HostAliases {
		if err := validateHostName("network.host_aliases", name); err != nil {
			return err
		}
		if strings.TrimSpace(addr) == "" {
			return fmt.Errorf("network.host_aliases entry %q has an empty address", name)
		}
		if net.ParseIP(strings.TrimSpace(addr)) == nil {
			return fmt.Errorf("network.host_aliases entry %q must map to an IP address, got %q", name, addr)
		}
	}

	// Validate cache paths (unkeyed) and entries (keyed overrides). Normalize
	// in place so downstream code (layer derivation, cache transfer) always
	// sees absolute guest paths.
	for i, p := range c.Cache.Paths {
		norm, err := validateCachePath("cache.paths", p)
		if err != nil {
			return err
		}
		c.Cache.Paths[i] = norm
	}
	for i, e := range c.Cache.Entries {
		norm, err := validateCachePath("cache.entries", e.Path)
		if err != nil {
			return err
		}
		c.Cache.Entries[i].Path = norm
	}

	// Validate environment variable names and values.
	for k, v := range c.Environment {
		if k == "" {
			return errors.New("environment contains empty key")
		}
		if !envNameRe.MatchString(k) {
			return fmt.Errorf("environment key %q is not a valid POSIX env-var name", k)
		}
		if strings.ContainsAny(v, "\x00\n") {
			return fmt.Errorf("environment value for %q must not contain NUL or newline", k)
		}
	}

	// Validate secret refs. Secrets are exposed as env vars in the VM, so each
	// name must be a valid POSIX env-var name. Duplicates and overlap with
	// `environment:` would shadow one another, so reject both. The scheme and
	// host scope are usage-site concerns validated here too.
	seenSecrets := make(map[string]bool, len(c.Secrets))
	for _, ref := range c.Secrets {
		if ref.Name == "" {
			return errors.New("secrets contains empty entry")
		}
		if !envNameRe.MatchString(ref.Name) {
			return fmt.Errorf("secret name %q is not a valid POSIX env-var name", ref.Name)
		}
		if seenSecrets[ref.Name] {
			return fmt.Errorf("secret name %q is duplicated", ref.Name)
		}
		if _, ok := c.Environment[ref.Name]; ok {
			return fmt.Errorf("secret name %q overlaps with environment key", ref.Name)
		}
		if !secretSchemes[ref.Scheme] {
			return fmt.Errorf("secret %q has invalid scheme %q: must be one of bearer, basic, oauth", ref.Name, ref.Scheme)
		}
		for _, host := range ref.Hosts {
			if err := validateHostPattern(fmt.Sprintf("secret %q hosts", ref.Name), host); err != nil {
				return err
			}
		}
		seenSecrets[ref.Name] = true
	}

	// Surface mode schema errors at load time.
	if len(c.Modes) > 0 {
		if _, err := c.Modes.Resolve(); err != nil {
			return fmt.Errorf("modes: %w", err)
		}
	}

	if err := c.PullRequest.validate(); err != nil {
		return fmt.Errorf("pull_request: %w", err)
	}

	// Preview state paths are checked against the cache paths, which the loop
	// above has already normalized, so the overlap check compares absolute guest
	// paths on both sides.
	cachePaths := make([]string, 0, len(c.Cache.Paths)+len(c.Cache.Entries))
	cachePaths = append(cachePaths, c.Cache.Paths...)
	for _, e := range c.Cache.Entries {
		cachePaths = append(cachePaths, e.Path)
	}
	if err := c.Preview.validate(cachePaths); err != nil {
		return fmt.Errorf("preview: %w", err)
	}

	allSteps := make([]Step, 0)
	allSteps = append(allSteps, c.Setup.Steps...)
	allSteps = append(allSteps, c.Setup.HealthChecks...)
	allSteps = append(allSteps, c.Validation.Required...)
	allSteps = append(allSteps, c.Validation.Advisory...)

	for _, s := range allSteps {
		if strings.TrimSpace(s.Name) == "" {
			return errors.New("step has empty name")
		}
		if strings.TrimSpace(s.Run) == "" {
			return fmt.Errorf("step %q has empty run command", s.Name)
		}
		if s.WorkingDir != "" && filepath.IsAbs(s.WorkingDir) {
			return fmt.Errorf("step %q has absolute working_dir %q (must be relative)", s.Name, s.WorkingDir)
		}
		const maxRetry = 10
		if s.Retry > maxRetry {
			return fmt.Errorf("step %q has retry count %d which exceeds maximum of %d", s.Name, s.Retry, maxRetry)
		}
	}

	return nil
}
