package project

import (
	"math"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
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

	// DefaultNixpkgsChannel is the nixpkgs channel resolved when the user
	// writes `nixpkgs` (no channel suffix). Bumped via kvarn release.
	DefaultNixpkgsChannel = "nixos-25.11"
)

// Config represents a project-level configuration file (kvarn.yml).
type Config struct {
	Image        string            `yaml:"image,omitempty"`
	Dependencies Dependencies      `yaml:"dependencies,omitempty"`
	VM           VM                `yaml:"vm"`
	Network      Network           `yaml:"network"`
	Cache        Cache             `yaml:"cache"`
	Environment  map[string]string `yaml:"environment,omitempty"`
	Secrets      []SecretRef       `yaml:"secrets,omitempty"`
	Setup        Setup             `yaml:"setup"`
	Validation   Validation        `yaml:"validation"`
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
//	"nixpkgs"           → github:NixOS/nixpkgs/<DefaultNixpkgsChannel>
//	"nixpkgs/<channel>" → github:NixOS/nixpkgs/<channel>
//	anything else       → flake URI verbatim
type Dependencies map[string][]string

// ResolvedDep is a single attribute-from-flake pair after schema resolution.
type ResolvedDep struct {
	FlakeURI string // canonical flake reference
	Attr     string // attribute path to install
	Host     string // hostname for firewall allowlist (may be empty)
}

// Resolve expands the source map into a flat slice of ResolvedDep entries.
// Each source is resolved to a canonical flake URI; each attribute is
// validated against nixAttrRe.
func (d Dependencies) Resolve() ([]ResolvedDep, error) {
	var out []ResolvedDep
	for source, attrs := range d {
		flakeURI, host, err := resolveFlakeRef(source)
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
				FlakeURI: flakeURI,
				Attr:     attr,
				Host:     host,
			})
		}
	}
	return out, nil
}

// resolveFlakeRef converts a user-facing source string into a canonical flake
// URI plus the hostname that needs firewall egress. Unknown forms are
// rejected with a friendly error.
func resolveFlakeRef(source string) (flakeURI, host string, err error) {
	s := strings.TrimSpace(source)
	if s == "" {
		return "", "", errors.New("dependency source must not be empty")
	}

	switch {
	case s == "nixpkgs":
		return "github:NixOS/nixpkgs/" + DefaultNixpkgsChannel, "github.com", nil

	case strings.HasPrefix(s, "nixpkgs/"):
		channel := strings.TrimPrefix(s, "nixpkgs/")
		if channel == "" {
			return "", "", fmt.Errorf("invalid nixpkgs source %q: channel must not be empty", source)
		}
		if !nixpkgsChannelRe.MatchString(channel) {
			return "", "", fmt.Errorf("invalid nixpkgs channel %q: must match %s",
				channel, nixpkgsChannelRe.String())
		}
		return "github:NixOS/nixpkgs/" + channel, "github.com", nil

	case strings.HasPrefix(s, "github:"):
		return s, "github.com", nil

	case strings.HasPrefix(s, "gitlab:"):
		return s, "gitlab.com", nil

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
			return "", "", fmt.Errorf("invalid dependency source %q: %v", source, err)
		}
		return s, u.Hostname(), nil
	}

	return "", "", fmt.Errorf("unsupported dependency source %q: expected `nixpkgs`, "+
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
		// for the legacy `tools:` key so users get a clear migration error
		// instead of silently losing their tool list.
		var raw map[string]yaml.Node
		if unmarshalErr := yaml.Unmarshal(data, &raw); unmarshalErr == nil {
			if _, ok := raw["tools"]; ok {
				return nil, fmt.Errorf("`tools:` has been replaced by `dependencies:` in %s; "+
					"see https://github.com/aholstenson/kvarn for migration", name)
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

// secretSchemes is the set of accepted kvarn.yml secret schemes. The empty
// string is accepted and defaults to bearer at resolution time.
var secretSchemes = map[string]bool{"": true, "bearer": true, "basic": true, "oauth": true}

func (c *Config) validate() error {
	// image and dependencies are mutually exclusive: shell sessions inside an
	// image: job run via `podman exec`, so host-installed Nix binaries are
	// invisible.
	if strings.TrimSpace(c.Image) != "" && len(c.Dependencies) > 0 {
		return errors.New("image and dependencies are mutually exclusive")
	}

	// Validate image is not whitespace-only if present.
	if c.Image != "" && strings.TrimSpace(c.Image) == "" {
		return errors.New("image must not be whitespace-only")
	}

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
