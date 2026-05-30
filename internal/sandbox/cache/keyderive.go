package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aholstenson/kvarn/internal/project"
)

// ToolEntry is the registry data DeriveLayers needs for one tool bucket. The
// sandbox package builds these from its tool registry and passes a LookupFunc
// in, so the cache package stays free of any dependency on sandbox.
type ToolEntry struct {
	Bucket     string
	Lockfiles  []string
	CachePaths []string
}

// LookupFunc resolves a nixpkgs dependency attribute to its ToolEntry, applying
// whatever version-suffix normalization the registry uses. It returns false
// when the attribute is not a registered tool.
type LookupFunc func(attr string) (ToolEntry, bool)

const nixpkgsPrefix = "github:NixOS/nixpkgs/"

// skipGlobDirs are never descended into when globbing for lockfiles: they hold
// vendored or build output whose nested lockfiles would pollute the input key.
var skipGlobDirs = map[string]bool{
	".git":        true,
	"node_modules": true,
	"vendor":      true,
	"target":      true,
	".venv":       true,
	"__pycache__": true,
}

// DeriveLayers maps a project's resolved dependencies and user cache config to
// the content-addressed cache layers to restore before and save after a job.
//
// Separator normalization and deterministic sorting ensure the orchestrator
// (Linux) and local runs (macOS) derive identical input keys for identical
// content.
func DeriveLayers(
	sourceDir string,
	deps []project.ResolvedDep,
	lookup LookupFunc,
	userCache project.Cache,
	projectID string,
	namespace string,
) ([]Layer, error) {
	var layers []Layer
	seenPath := map[string]bool{}

	add := func(bucket, guestPath, inputKey string) {
		if guestPath == "" || seenPath[guestPath] {
			return
		}
		seenPath[guestPath] = true
		layers = append(layers, Layer{
			Key: Key{
				ProjectID: projectID,
				Namespace: namespace,
				Bucket:    bucket,
				GuestPath: guestPath,
				InputKey:  inputKey,
			},
			GuestPath: guestPath,
		})
	}

	// Deterministic dep order so bucket/path claims and channel folding don't
	// depend on map iteration order.
	sorted := append([]project.ResolvedDep(nil), deps...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].FlakeURI != sorted[j].FlakeURI {
			return sorted[i].FlakeURI < sorted[j].FlakeURI
		}
		return sorted[i].Attr < sorted[j].Attr
	})

	for _, d := range sorted {
		if !strings.HasPrefix(d.FlakeURI, nixpkgsPrefix) {
			continue
		}
		entry, ok := lookup(d.Attr)
		if !ok || len(entry.CachePaths) == 0 {
			continue
		}
		channel := strings.TrimPrefix(d.FlakeURI, nixpkgsPrefix)
		inputKey, err := deriveToolInputKey(sourceDir, entry.Bucket, entry.Lockfiles, channel)
		if err != nil {
			return nil, err
		}
		for _, p := range entry.CachePaths {
			add(entry.Bucket, p, inputKey)
		}
	}

	// Nix eval/fetch cache: cheap to restore, keyed by the dep set and channels
	// so any dependency or channel change invalidates it.
	if nixKey, ok := deriveNixEvalInputKey(sorted); ok {
		add("nix-eval", "/home/kvarn/.cache/nix", nixKey)
	}

	// User-configured cache (power-user overrides).
	for _, p := range userCache.Paths {
		bucket := "user:" + flattenPath(p)
		add(bucket, p, unkeyedInputKey(bucket))
	}
	for _, e := range userCache.Entries {
		bucket := e.Bucket
		if bucket == "" {
			bucket = "user:" + flattenPath(e.Path)
		}
		var inputKey string
		switch {
		case e.Key != "":
			inputKey = manualInputKey(e.Key)
		case len(e.Lockfiles) > 0:
			ik, err := deriveToolInputKey(sourceDir, bucket, e.Lockfiles, "")
			if err != nil {
				return nil, err
			}
			inputKey = ik
		default:
			inputKey = unkeyedInputKey(bucket)
		}
		add(bucket, e.Path, inputKey)
	}

	return layers, nil
}

// deriveToolInputKey content-addresses a bucket by its lockfiles and channel.
// A missing lockfile yields a degraded channel-only key so warm starts still
// work; a lockfile appearing later simply mints a new key.
func deriveToolInputKey(sourceDir, bucket string, lockfiles []string, channel string) (string, error) {
	digest, ok, err := hashLockfiles(sourceDir, lockfiles)
	if err != nil {
		return "", err
	}
	if !ok {
		return hexSum(bucket + "-nolock||" + channel), nil
	}
	return hexSum(digest + "||" + channel), nil
}

func unkeyedInputKey(bucket string) string {
	return hexSum(bucket + "-nolock")
}

func manualInputKey(key string) string {
	return hexSum("manual||" + key)
}

// deriveNixEvalInputKey keys the nix eval/fetch cache by the sorted nixpkgs
// dependency set and the set of channels they resolve from.
func deriveNixEvalInputKey(deps []project.ResolvedDep) (string, bool) {
	var refs []string
	channels := map[string]bool{}
	for _, d := range deps {
		if !strings.HasPrefix(d.FlakeURI, nixpkgsPrefix) {
			continue
		}
		refs = append(refs, d.FlakeURI+"#"+d.Attr)
		channels[strings.TrimPrefix(d.FlakeURI, nixpkgsPrefix)] = true
	}
	if len(refs) == 0 {
		return "", false
	}
	sort.Strings(refs)
	chs := make([]string, 0, len(channels))
	for c := range channels {
		chs = append(chs, c)
	}
	sort.Strings(chs)
	return hexSum(strings.Join(refs, "\n") + "||" + strings.Join(chs, ",")), true
}

// hashLockfiles globs sourceDir for the given lockfile patterns and returns a
// digest over the matched files. Location matters: each record is
// "<relpath>\0<sha256(content)>", sorted, so a monorepo's per-module lockfiles
// produce a stable, position-sensitive digest. ok is false when nothing
// matched.
func hashLockfiles(sourceDir string, globs []string) (digest string, ok bool, err error) {
	if len(globs) == 0 || sourceDir == "" {
		return "", false, nil
	}
	var records []string
	seen := map[string]bool{}
	walkErr := filepath.WalkDir(sourceDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != sourceDir && skipGlobDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(sourceDir, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !matchesAnyGlob(rel, d.Name(), globs) || seen[rel] {
			return nil
		}
		seen[rel] = true
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		records = append(records, rel+"\x00"+hexSumBytes(content))
		return nil
	})
	if walkErr != nil {
		return "", false, walkErr
	}
	if len(records) == 0 {
		return "", false, nil
	}
	sort.Strings(records)
	return hexSum(strings.Join(records, "\n")), true, nil
}

// matchesAnyGlob reports whether a file matches any of the patterns. A leading
// "**/" matches the basename at any depth; other patterns match the full
// relative path. All comparisons use forward slashes.
func matchesAnyGlob(rel, base string, globs []string) bool {
	for _, g := range globs {
		g = filepath.ToSlash(g)
		if strings.HasPrefix(g, "**/") {
			pat := g[3:]
			if matched, _ := filepath.Match(pat, base); matched {
				return true
			}
			if matched, _ := filepath.Match(pat, rel); matched {
				return true
			}
			continue
		}
		if matched, _ := filepath.Match(g, rel); matched {
			return true
		}
	}
	return false
}

func hexSum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func hexSumBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// flattenPath converts an absolute guest path into a flat bucket-name suffix.
// e.g. "/home/kvarn/.cache/foo" -> "home_kvarn_.cache_foo"
func flattenPath(p string) string {
	return strings.ReplaceAll(strings.TrimPrefix(p, "/"), "/", "_")
}
