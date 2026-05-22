package buildinfo

import (
	"regexp"

	"github.com/Masterminds/semver/v3"
)

// Version is the release version, injected at build time via -ldflags.
// "dev" (the default) means an unversioned local build.
var Version = "dev"

// Repo hosts the release assets the CLI downloads from.
const Repo = "aholstenson/kvarn"

// ImageConstraint is the compiled-in semver range of VM images this CLI build
// is compatible with. The orchestrator embeds and ships the exact runner it
// speaks to, so runner↔orchestrator skew is impossible by construction; the
// only remaining contract is the coarser base-OS/tooling ABI of the image,
// expressed here and resolved to a concrete published image at runtime. Bump
// it deliberately when the runner needs a newer image contract.
var ImageConstraint = ">=0.1.0 <0.2.0"

var releaseRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)

// IsRelease reports whether v is a clean release tag (vX.Y.Z), i.e. one that
// has published assets. Local/dirty git-describe versions return false.
func IsRelease(v string) bool { return releaseRe.MatchString(v) }

// IsVersionRange reports whether v is a semver constraint range (e.g.
// ">=0.1.0 <0.2.0") rather than a single concrete version ("0.1.0"/"v0.1.0").
// A concrete version parses as a semver value; anything else is treated as a
// range to resolve against the published image manifest.
func IsVersionRange(v string) bool {
	if v == "" {
		return false
	}
	_, err := semver.NewVersion(v)
	return err != nil
}
