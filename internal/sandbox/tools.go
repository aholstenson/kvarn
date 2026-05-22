package sandbox

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"fmt"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/project"
)

// defaultCachePaths are always added to cachePaths in VM mode, regardless
// of whether dependencies are configured. /home/kvarn/.cache is the XDG
// default and catches most well-behaved tools (pip, sccache, modern Go
// build cache, etc.) without per-tool curation.
var defaultCachePaths = []string{"/home/kvarn/.cache"}

// toolEntry describes the host-side settings applied to the sandbox when a
// nixpkgs attribute is installed.
type toolEntry struct {
	CachePaths         []string
	Hosts              []string
	Env                map[string]string
	PathPrepend        []string
	StripVersionSuffix bool
}

// toolRegistry maps normalized nixpkgs attr names to curation entries.
// Consulted only for deps whose FlakeURI is rooted at github:NixOS/nixpkgs/.
var toolRegistry = map[string]toolEntry{
	"go": {
		CachePaths:         []string{"/home/kvarn/go"},
		Hosts:              []string{"proxy.golang.org", "sum.golang.org", "storage.googleapis.com"},
		Env:                map[string]string{"GOPATH": "/home/kvarn/go"},
		PathPrepend:        []string{"/home/kvarn/go/bin"},
		StripVersionSuffix: true,
	},
	"nodejs": {
		CachePaths:         []string{"/home/kvarn/.npm"},
		Hosts:              []string{"registry.npmjs.org", "nodejs.org"},
		StripVersionSuffix: true,
	},
	"cargo": {
		CachePaths:  []string{"/home/kvarn/.cargo"},
		Hosts:       []string{"crates.io", "static.crates.io", "index.crates.io"},
		Env:         map[string]string{"CARGO_HOME": "/home/kvarn/.cargo"},
		PathPrepend: []string{"/home/kvarn/.cargo/bin"},
	},
	"rustc": {
		CachePaths:  []string{"/home/kvarn/.cargo"},
		Hosts:       []string{"crates.io", "static.crates.io", "index.crates.io"},
		Env:         map[string]string{"CARGO_HOME": "/home/kvarn/.cargo"},
		PathPrepend: []string{"/home/kvarn/.cargo/bin"},
	},
	"python3": {
		Hosts:              []string{"pypi.org", "files.pythonhosted.org"},
		StripVersionSuffix: true,
	},
	"python": {
		Hosts:              []string{"pypi.org", "files.pythonhosted.org"},
		StripVersionSuffix: true,
	},
	"bun": {
		CachePaths: []string{"/home/kvarn/.bun/install/cache"},
		Hosts:      []string{"bun.sh", "registry.npmjs.org"},
	},
	"openjdk": {
		CachePaths:         []string{"/home/kvarn/.gradle", "/home/kvarn/.m2"},
		Hosts:              []string{"repo.maven.apache.org", "repo1.maven.org", "services.gradle.org"},
		StripVersionSuffix: true,
	},
	"ruby": {
		CachePaths:         []string{"/home/kvarn/.gem"},
		Hosts:              []string{"rubygems.org"},
		StripVersionSuffix: true,
	},
	"deno": {
		Hosts: []string{"deno.land", "jsr.io"},
		Env:   map[string]string{"DENO_DIR": "/home/kvarn/.cache/deno"},
	},
	"buf": {
		Hosts: []string{"buf.build"},
	},
}

// versionSuffixRe matches trailing _NN(_NN)? style suffixes (e.g. `go_1_22`).
var versionSuffixRe = regexp.MustCompile(`_[0-9]+(_[0-9]+)*$`)

// trailingDigitsRe matches a trailing run of digits (e.g. `python312`).
var trailingDigitsRe = regexp.MustCompile(`[0-9]+$`)

// lookupTool returns the tool entry for a nixpkgs attr, or zero entry
// + false. Tries exact match first; if absent and the would-be entry opts
// in via StripVersionSuffix, retries with `_NN(_NN)?` and trailing-digit
// forms stripped.
func lookupTool(attr string) (toolEntry, bool) {
	if e, ok := toolRegistry[attr]; ok {
		return e, true
	}

	stripped := versionSuffixRe.ReplaceAllString(attr, "")
	if stripped != attr {
		if e, ok := toolRegistry[stripped]; ok && e.StripVersionSuffix {
			return e, true
		}
	}

	stripped2 := trailingDigitsRe.ReplaceAllString(attr, "")
	if stripped2 != attr {
		if e, ok := toolRegistry[stripped2]; ok && e.StripVersionSuffix {
			return e, true
		}
	}

	return toolEntry{}, false
}

// augmentations is the merged result of consulting toolRegistry for each
// nixpkgs dep, deduplicated across deps.
type augmentations struct {
	CachePaths  []string
	Hosts       []string
	Env         map[string]string
	PathPrepend []string
}

// computeAugmentations consults toolRegistry for each nixpkgs dep and
// merges results. Non-nixpkgs flake URIs are skipped. Cache paths and PATH
// prepends are deduplicated preserving order; env vars merge with later
// tool entries overriding earlier ones (order is non-deterministic; users
// are expected to use the explicit `environment:` field for overrides).
func computeAugmentations(deps []project.ResolvedDep) augmentations {
	var aug augmentations
	cacheSeen := make(map[string]bool)
	hostSeen := make(map[string]bool)
	pathSeen := make(map[string]bool)

	for _, d := range deps {
		if !strings.HasPrefix(d.FlakeURI, "github:NixOS/nixpkgs/") {
			continue
		}
		entry, ok := lookupTool(d.Attr)
		if !ok {
			continue
		}
		for _, p := range entry.CachePaths {
			if !cacheSeen[p] {
				cacheSeen[p] = true
				aug.CachePaths = append(aug.CachePaths, p)
			}
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
				aug.PathPrepend = append(aug.PathPrepend, p)
			}
		}
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

// writeProfileScripts uploads /etc/profile.d/kvarn-tools.sh (tool entries
// from the registry), /etc/profile.d/kvarn-user.sh (user `environment:`
// values), and /etc/profile.d/kvarn-secrets.sh (resolved secrets, possibly
// containing bearer placeholders). Files are named so lexical order
// (tools → user → secrets) matches the desired sourcing order; later
// assignments win, so secrets override user values which override tools.
// The secrets file is uploaded with mode 0600 since the runner shell is
// the only consumer that needs to source it. Files with empty content are
// skipped.
func writeProfileScripts(ctx context.Context, proxy RunnerProxy, aug augmentations, userEnv, secrets map[string]string) error {
	tools := buildProfileScript(aug.Env, aug.PathPrepend)
	user := buildProfileScript(userEnv, nil)
	secretScript := buildProfileScript(secrets, nil)

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
