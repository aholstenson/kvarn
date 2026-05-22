package buildinfo

import "regexp"

// Version is the release version, injected at build time via -ldflags.
// "dev" (the default) means an unversioned local build.
var Version = "dev"

// Repo hosts the release assets the CLI downloads from.
const Repo = "aholstenson/kvarn"

var releaseRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)

// IsRelease reports whether v is a clean release tag (vX.Y.Z), i.e. one that
// has published assets. Local/dirty git-describe versions return false.
func IsRelease(v string) bool { return releaseRe.MatchString(v) }
