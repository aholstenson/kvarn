package sandbox

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"fmt"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox/cache"
)

// toolEntry describes the host-side settings applied to the sandbox when a
// nixpkgs attribute is installed. Every field is a claim about what the tool
// needs to work offline-ish and fast: where it writes state worth keeping, what
// invalidates that state, which hosts it fetches from, and how its environment
// has to look.
//
// Adding an entry here is the whole of "kvarn supports tool X" — see
// docs/reference/registered-tools.md, which a test keeps in step with this map.
type toolEntry struct {
	CachePaths []string
	// Lockfiles are glob patterns (relative to the source dir) whose contents
	// content-address this tool's cache. A leading "**/" matches at any depth,
	// making the keying monorepo-aware.
	Lockfiles   []string
	Hosts       []string
	Env         map[string]string
	PathPrepend []string
	// PathPriority orders this entry's PathPrepend against every other tool's.
	// Higher lands closer to the front of PATH. It exists for tools that shim
	// binaries other tools also provide: a version manager has to win over the
	// directories the tools it manages install into, or its pins do nothing.
	// Ties resolve by attribute name, so the result never depends on map order.
	PathPriority int
	// Provision are shell commands run once the VM is up and the environment is
	// in place, before any setup step. They exist for tools whose declaration is
	// only half the story — a version manager installs nothing until told to,
	// and its shims are an empty directory until it has. Each must be safe to
	// run on a warm cache and in a repository that does not use the tool.
	Provision          []string
	StripVersionSuffix bool
}

// toolRegistry maps normalized nixpkgs attr names to curation entries.
// Consulted only for deps whose FlakeURI is rooted at github:NixOS/nixpkgs/.
//
// Several tools claim the same cache path from different angles — `openjdk`
// covers a project that declares only a JDK and drives it with a wrapper
// script, while `gradle` and `maven` key the same directories on the build
// files that actually change. Declaring both is fine: cache layer derivation
// walks dependencies sorted by flake source then attribute, and the first claim
// on a path wins, so the build tool's sharper key beats the JDK's catch-all.
var toolRegistry = map[string]toolEntry{
	"go": {
		// The module cache under GOPATH is keyed by go.sum; the build cache is
		// keyed the same way for lack of anything better, and still pays for
		// itself — a warm build cache is the difference between recompiling the
		// dependency graph and recompiling the packages that changed.
		CachePaths:         []string{"/home/kvarn/go", "/home/kvarn/.cache/go-build"},
		Lockfiles:          []string{"**/go.sum", "**/go.mod"},
		Hosts:              []string{"proxy.golang.org", "sum.golang.org", "storage.googleapis.com"},
		Env:                map[string]string{"GOPATH": "/home/kvarn/go"},
		PathPrepend:        []string{"/home/kvarn/go/bin"},
		StripVersionSuffix: true,
	},
	"golangci-lint": {
		CachePaths: []string{"/home/kvarn/.cache/golangci-lint"},
		Lockfiles:  []string{"**/.golangci.yml", "**/.golangci.yaml", "**/.golangci.toml"},
		Hosts:      []string{"proxy.golang.org", "sum.golang.org"},
	},
	"nodejs": {
		CachePaths:         []string{"/home/kvarn/.npm"},
		Lockfiles:          []string{"**/package-lock.json", "**/npm-shrinkwrap.json"},
		Hosts:              []string{"registry.npmjs.org", "nodejs.org"},
		StripVersionSuffix: true,
	},
	"pnpm": {
		// pnpm's content-addressed store is what makes it worth caching; it
		// lives under PNPM_HOME, which also holds the global bin directory.
		CachePaths:  []string{"/home/kvarn/.local/share/pnpm/store"},
		Lockfiles:   []string{"**/pnpm-lock.yaml"},
		Hosts:       []string{"registry.npmjs.org"},
		Env:         map[string]string{"PNPM_HOME": "/home/kvarn/.local/share/pnpm"},
		PathPrepend: []string{"/home/kvarn/.local/share/pnpm"},
	},
	"yarn": {
		CachePaths: []string{"/home/kvarn/.cache/yarn"},
		Lockfiles:  []string{"**/yarn.lock"},
		Hosts:      []string{"registry.yarnpkg.com", "registry.npmjs.org"},
	},
	"cargo": {
		CachePaths:  []string{"/home/kvarn/.cargo"},
		Lockfiles:   []string{"**/Cargo.lock"},
		Hosts:       []string{"crates.io", "static.crates.io", "index.crates.io"},
		Env:         map[string]string{"CARGO_HOME": "/home/kvarn/.cargo"},
		PathPrepend: []string{"/home/kvarn/.cargo/bin"},
	},
	"rustc": {
		CachePaths:  []string{"/home/kvarn/.cargo"},
		Lockfiles:   []string{"**/Cargo.lock"},
		Hosts:       []string{"crates.io", "static.crates.io", "index.crates.io"},
		Env:         map[string]string{"CARGO_HOME": "/home/kvarn/.cargo"},
		PathPrepend: []string{"/home/kvarn/.cargo/bin"},
	},
	"python3": {
		CachePaths:         []string{"/home/kvarn/.cache/pip"},
		Lockfiles:          []string{"**/requirements*.txt", "**/poetry.lock", "**/uv.lock"},
		Hosts:              []string{"pypi.org", "files.pythonhosted.org"},
		StripVersionSuffix: true,
	},
	"python": {
		CachePaths:         []string{"/home/kvarn/.cache/pip"},
		Lockfiles:          []string{"**/requirements*.txt", "**/poetry.lock", "**/uv.lock"},
		Hosts:              []string{"pypi.org", "files.pythonhosted.org"},
		StripVersionSuffix: true,
	},
	"uv": {
		// uv downloads its own interpreters (python-build-standalone, hosted on
		// GitHub release assets) into the data directory, so that is cached
		// alongside the package cache.
		CachePaths: []string{"/home/kvarn/.cache/uv", "/home/kvarn/.local/share/uv"},
		Lockfiles:  []string{"**/uv.lock", "**/pyproject.toml", "**/requirements*.txt"},
		Hosts: []string{
			"pypi.org", "files.pythonhosted.org",
			"api.github.com", "objects.githubusercontent.com",
		},
		Env: map[string]string{
			"UV_CACHE_DIR":          "/home/kvarn/.cache/uv",
			"UV_PYTHON_INSTALL_DIR": "/home/kvarn/.local/share/uv/python",
		},
	},
	"bun": {
		CachePaths: []string{"/home/kvarn/.bun/install/cache"},
		Lockfiles:  []string{"**/bun.lockb", "**/bun.lock", "**/package-lock.json"},
		Hosts:      []string{"bun.sh", "registry.npmjs.org"},
	},
	"openjdk": {
		CachePaths:         []string{"/home/kvarn/.gradle", "/home/kvarn/.m2"},
		Lockfiles:          []string{"**/gradle.lockfile", "**/pom.xml"},
		Hosts:              []string{"repo.maven.apache.org", "repo1.maven.org", "services.gradle.org"},
		StripVersionSuffix: true,
	},
	"gradle": {
		CachePaths: []string{"/home/kvarn/.gradle"},
		Lockfiles: []string{
			"**/gradle.lockfile", "**/gradle-wrapper.properties",
			"**/build.gradle", "**/build.gradle.kts",
			"**/settings.gradle", "**/settings.gradle.kts",
			"**/libs.versions.toml",
		},
		Hosts:              []string{"services.gradle.org", "repo.maven.apache.org", "repo1.maven.org", "plugins.gradle.org"},
		StripVersionSuffix: true,
	},
	"maven": {
		CachePaths:         []string{"/home/kvarn/.m2"},
		Lockfiles:          []string{"**/pom.xml"},
		Hosts:              []string{"repo.maven.apache.org", "repo1.maven.org"},
		StripVersionSuffix: true,
	},
	"ruby": {
		CachePaths:         []string{"/home/kvarn/.gem"},
		Lockfiles:          []string{"**/Gemfile.lock"},
		Hosts:              []string{"rubygems.org"},
		StripVersionSuffix: true,
	},
	"deno": {
		CachePaths: []string{"/home/kvarn/.cache/deno"},
		Hosts:      []string{"deno.land", "jsr.io"},
		Env:        map[string]string{"DENO_DIR": "/home/kvarn/.cache/deno"},
	},
	"pre-commit": {
		// Every hook gets its own language environment built from scratch on a
		// cold cache, which is minutes of work keyed entirely by one file.
		CachePaths: []string{"/home/kvarn/.cache/pre-commit"},
		Lockfiles:  []string{"**/.pre-commit-config.yaml"},
		Hosts:      []string{"pypi.org", "files.pythonhosted.org", "registry.npmjs.org"},
	},
	"mise": {
		// mise installs nothing on its own, so the shims directory is empty
		// until `mise install` has run — hence the provisioning command. The
		// shims outrank every other tool's bin directory because a repository
		// that pins a toolchain in mise.toml means that pin to win, including
		// over a nixpkgs attribute declared alongside it for its cache curation.
		CachePaths:   []string{"/home/kvarn/.local/share/mise", "/home/kvarn/.cache/mise"},
		Lockfiles:    []string{"**/mise.toml", "**/.mise.toml", "**/mise.lock", "**/.tool-versions"},
		PathPrepend:  []string{"/home/kvarn/.local/share/mise/shims"},
		PathPriority: 100,
		Provision:    []string{"mise install"},
		Env: map[string]string{
			// mise refuses to read a config it has not been told to trust, and
			// there is no one here to answer the prompt.
			"MISE_TRUSTED_CONFIG_PATHS": project.GuestWorkspace,
			"MISE_YES":                  "1",
		},
		// mise resolves most tools through GitHub releases, but its core
		// backends fetch a language's official distribution instead. Go and
		// Node are the two that come up constantly; anything else a repository
		// pins goes in network.allowed_hosts, and the error names the host.
		Hosts: []string{
			"api.github.com", "raw.githubusercontent.com",
			"mise-versions.jdx.dev",
			"dl.google.com", "nodejs.org",
		},
	},
	"buf": {
		Hosts: []string{"buf.build"},
	},
}

// toolProvisionTimeoutSeconds caps a single provisioning command. A cold
// `mise install` builds or downloads whole toolchains, so the ceiling is
// generous; it is here to stop a hung command from holding the VM open, not to
// bound normal work.
const toolProvisionTimeoutSeconds uint32 = 900

// versionSuffixRe matches trailing _NN(_NN)? style suffixes (e.g. `go_1_22`).
var versionSuffixRe = regexp.MustCompile(`_[0-9]+(_[0-9]+)*$`)

// trailingDigitsRe matches a trailing run of digits (e.g. `python312`).
var trailingDigitsRe = regexp.MustCompile(`[0-9]+$`)

// lookupTool returns the tool entry for a nixpkgs attr, or zero entry + false.
func lookupTool(attr string) (toolEntry, bool) {
	_, e, ok := lookupToolNamed(attr)
	return e, ok
}

// lookupToolNamed resolves a nixpkgs attr to its canonical registry name and
// entry. Tries an exact match first; if absent and the would-be entry opts in
// via StripVersionSuffix, retries with `_NN(_NN)?` and trailing-digit forms
// stripped. The returned name is the registry key, so versioned attrs (e.g.
// `go_1_22`) map to a stable bucket (`go`).
func lookupToolNamed(attr string) (string, toolEntry, bool) {
	if e, ok := toolRegistry[attr]; ok {
		return attr, e, true
	}

	stripped := versionSuffixRe.ReplaceAllString(attr, "")
	if stripped != attr {
		if e, ok := toolRegistry[stripped]; ok && e.StripVersionSuffix {
			return stripped, e, true
		}
	}

	stripped2 := trailingDigitsRe.ReplaceAllString(attr, "")
	if stripped2 != attr {
		if e, ok := toolRegistry[stripped2]; ok && e.StripVersionSuffix {
			return stripped2, e, true
		}
	}

	return "", toolEntry{}, false
}

// cacheToolLookup adapts the tool registry to cache.LookupFunc so the cache
// package can content-address tool caches without importing sandbox.
func cacheToolLookup(attr string) (cache.ToolEntry, bool) {
	name, e, ok := lookupToolNamed(attr)
	if !ok {
		return cache.ToolEntry{}, false
	}
	return cache.ToolEntry{
		Bucket:     name,
		Lockfiles:  e.Lockfiles,
		CachePaths: e.CachePaths,
	}, true
}

// provisionStep is one command a registered tool needs run before the job
// starts, tagged with the tool that asked for it so the session log can say
// what is taking the time.
type provisionStep struct {
	Tool    string
	Command string
}

// augmentations is the merged result of consulting toolRegistry for each
// nixpkgs dep, deduplicated across deps.
type augmentations struct {
	Hosts []string
	Env   map[string]string
	// PathPrepend is ordered so that writing it out one prepend per line leaves
	// the highest-priority directory at the front of PATH.
	PathPrepend []string
	Provision   []provisionStep
}

// computeAugmentations consults toolRegistry for each nixpkgs dep and merges
// the results. Non-nixpkgs flake URIs are skipped, and hosts, PATH entries and
// provisioning commands are deduplicated.
//
// Dependencies arrive from a map, so they are sorted by attribute before
// anything is merged: two tools can claim the same env var or want the same
// directory on PATH, and which one wins must not change between runs of the
// same config.
func computeAugmentations(deps []project.ResolvedDep) augmentations {
	var aug augmentations
	hostSeen := make(map[string]bool)
	pathSeen := make(map[string]bool)
	provisionSeen := make(map[string]bool)

	// prepends collects PATH entries with the priority that orders them; the
	// sort happens once, after every dep has had its say.
	type prepend struct {
		priority int
		dir      string
	}
	var prepends []prepend

	sorted := append([]project.ResolvedDep(nil), deps...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Attr != sorted[j].Attr {
			return sorted[i].Attr < sorted[j].Attr
		}
		return sorted[i].FlakeURI < sorted[j].FlakeURI
	})

	for _, d := range sorted {
		if !strings.HasPrefix(d.FlakeURI, project.NixpkgsFlakePrefix) {
			continue
		}
		name, entry, ok := lookupToolNamed(d.Attr)
		if !ok {
			continue
		}
		for _, h := range entry.Hosts {
			if !hostSeen[h] {
				hostSeen[h] = true
				aug.Hosts = append(aug.Hosts, h)
			}
		}
		for k, v := range entry.Env {
			if aug.Env == nil {
				aug.Env = make(map[string]string)
			}
			aug.Env[k] = v
		}
		for _, p := range entry.PathPrepend {
			if !pathSeen[p] {
				pathSeen[p] = true
				prepends = append(prepends, prepend{priority: entry.PathPriority, dir: p})
			}
		}
		for _, c := range entry.Provision {
			if !provisionSeen[c] {
				provisionSeen[c] = true
				aug.Provision = append(aug.Provision, provisionStep{Tool: name, Command: c})
			}
		}
	}

	// Ascending priority: the profile script prepends line by line, so the last
	// line written ends up first on PATH. SliceStable keeps attribute order
	// within a priority.
	sort.SliceStable(prepends, func(i, j int) bool { return prepends[i].priority < prepends[j].priority })
	for _, p := range prepends {
		aug.PathPrepend = append(aug.PathPrepend, p.dir)
	}
	return aug
}

// buildProfileScript renders a /etc/profile.d snippet that exports the
// given env vars and prepends the given dirs to PATH. Output is empty if
// both inputs are empty. Env keys are sorted for deterministic output.
func buildProfileScript(env map[string]string, pathPrepend []string) string {
	if len(env) == 0 && len(pathPrepend) == 0 {
		return ""
	}

	var b strings.Builder
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("export ")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(shellQuote(env[k]))
		b.WriteString("\n")
	}
	for _, dir := range pathPrepend {
		b.WriteString("export PATH=")
		b.WriteString(shellQuote(dir))
		b.WriteString(":\"$PATH\"\n")
	}
	return b.String()
}

// writeProfileScripts uploads /etc/profile.d/kvarn-tools.sh (tool environment
// from the registry), /etc/profile.d/kvarn-user.sh (user `environment:`
// values), /etc/profile.d/kvarn-secrets.sh (resolved secrets, possibly
// containing bearer placeholders), and /etc/profile.d/zz-kvarn-path.sh (the
// registry's PATH entries).
//
// The names encode the sourcing order, because /etc/profile globs the
// directory and sh sources what it finds in sorted order. Environment
// assignments run tools → user → secrets, so a later one wins: a secret
// overrides a user value, which overrides a tool default.
//
// PATH is separate and last on purpose. The image links the Nix profile in as
// /etc/profile.d/nix.sh, which prepends ~/.nix-profile/bin; anything kvarn
// writes under a `kvarn-` name sorts ahead of it and would be pushed behind
// the Nix profile. A version manager's shims have to sit in front of the
// binaries they shadow, so the PATH file is named to sort after nix.sh.
//
// The secrets file is uploaded with mode 0600 since the runner shell is the
// only consumer that needs to source it. Files with empty content are skipped.
func writeProfileScripts(ctx context.Context, proxy RunnerProxy, aug augmentations, userEnv, secrets map[string]string) error {
	tools := buildProfileScript(aug.Env, nil)
	user := buildProfileScript(userEnv, nil)
	secretScript := buildProfileScript(secrets, nil)
	pathScript := buildProfileScript(nil, aug.PathPrepend)

	var files []*v1.FileContent
	if tools != "" {
		files = append(files, &v1.FileContent{
			Path:    "kvarn-tools.sh",
			Content: []byte(tools),
			Mode:    0o644,
		})
	}
	if user != "" {
		files = append(files, &v1.FileContent{
			Path:    "kvarn-user.sh",
			Content: []byte(user),
			Mode:    0o644,
		})
	}
	if secretScript != "" {
		files = append(files, &v1.FileContent{
			Path:    "kvarn-secrets.sh",
			Content: []byte(secretScript),
			Mode:    0o600,
		})
	}
	if pathScript != "" {
		files = append(files, &v1.FileContent{
			Path:    "zz-kvarn-path.sh",
			Content: []byte(pathScript),
			Mode:    0o644,
		})
	}
	if len(files) == 0 {
		return nil
	}

	if _, err := proxy.UploadFiles(ctx, &v1.UploadFilesRequest{
		WorkingDir: "/etc/profile.d",
		Files:      files,
	}); err != nil {
		return fmt.Errorf("upload profile.d scripts: %w", err)
	}
	return nil
}

// provisionTool runs one registered tool's provisioning command in the job's
// shell session, which is where it belongs: the session is a login shell, so
// the command sees the profile.d environment written moments earlier, and any
// state it leaves behind is state the setup steps and the agent inherit.
func provisionTool(ctx context.Context, runner RunnerProxy, sessionID string, step provisionStep, onOutput OutputCallback) error {
	resp, err := runner.SessionExec(ctx, &v1.SessionExecRequest{
		SessionId:      sessionID,
		Command:        step.Command,
		TimeoutSeconds: toolProvisionTimeoutSeconds,
	}, onOutput)
	if err != nil {
		return fmt.Errorf("provision %s: %w", step.Tool, err)
	}
	if resp.TimedOut {
		return fmt.Errorf("provision %s: %q timed out after %ds: %s",
			step.Tool, step.Command, toolProvisionTimeoutSeconds, strings.TrimSpace(resp.Stderr))
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("provision %s: %q failed (exit %s): %s",
			step.Tool, step.Command, formatExitCode(resp.ExitCode), strings.TrimSpace(resp.Stderr))
	}
	return nil
}
