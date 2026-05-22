package vm

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/aholstenson/kvarn/internal/buildinfo"
	"github.com/cockroachdb/errors"
)

// searchPaths are well-known locations to look for the disk image, in order.
// The first match wins. Each path is joined with the image subpath
// (e.g. "arm64/disk.qcow2").
var searchPaths = []string{
	"/usr/local/share/kvarn/dist",
	"/opt/kvarn/dist",
}

// executable locates the running binary. It is a var so tests can control the
// binary-relative dist/ search location.
var executable = os.Executable

// DownloadOpts controls how EnsureDiskImage resolves and, if necessary,
// downloads the VM disk image.
type DownloadOpts struct {
	// Path is an explicit disk image path. When set it is used verbatim, with
	// no resolution or download.
	Path string
	// Version selects which released image to use. Empty falls back to the
	// KVARN_IMAGE_VERSION env var, then the CLI build version.
	Version string
	// Arch selects the image architecture. Empty defaults to runtime.GOARCH.
	Arch string
	// ForceDownload skips local/cache resolution and always downloads.
	ForceDownload bool
	// NoDownload restricts resolution to local and cached images; a missing
	// image is an error instead of a download.
	NoDownload bool
	// Progress, if set, is called as the image downloads with the number of
	// bytes fetched so far and the total (-1 if unknown).
	Progress func(done, total int64)
}

// localDiskImagePath checks the binary-relative dist/ dir and the well-known
// system paths for the current architecture's disk image. It returns the
// resolved path (or "") and the list of locations checked for error messages.
func localDiskImagePath() (string, []string) {
	sub := filepath.Join(runtime.GOARCH, "disk.qcow2")
	var checked []string

	if execPath, err := executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(execPath), "dist", sub)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, checked
		}
		checked = append(checked, candidate)
	}

	for _, dir := range searchPaths {
		candidate := filepath.Join(dir, sub)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, checked
		}
		checked = append(checked, candidate)
	}

	return "", checked
}

// ResolveDiskImagePath finds an already-present disk image for the current
// architecture without downloading. It checks, in order:
//  1. Relative to the running binary (<binary-dir>/dist/<arch>/disk.qcow2)
//  2. Well-known system paths
//  3. The per-version cache for the CLI build version
//
// Returns the resolved absolute path, or an error listing the locations
// checked. Use EnsureDiskImage to additionally download a missing release
// image.
func ResolveDiskImagePath() (string, error) {
	path, checked := localDiskImagePath()
	if path != "" {
		return path, nil
	}

	if cached, err := cachedDiskImagePath(buildinfo.Version, runtime.GOARCH); err == nil {
		if _, statErr := os.Stat(cached); statErr == nil {
			return cached, nil
		}
		checked = append(checked, cached)
	}

	return "", errors.Newf(
		"could not find disk image for %s in any of:\n  %s",
		runtime.GOARCH,
		strings.Join(checked, "\n  "),
	)
}

// EnsureDiskImage resolves the VM disk image, downloading a released image into
// the user cache when no local copy is found. Resolution order:
//  1. opts.Path explicit override
//  2. local dist/ + well-known system paths
//  3. per-version user cache
//  4. download from the GitHub release (released versions, an explicit version
//     override, or opts.ForceDownload)
//
// The effective version defaults to the CLI build version, overridden by the
// KVARN_IMAGE_VERSION env var, then opts.Version. A non-release version with no
// local image yields an error pointing at `task image:build`.
func EnsureDiskImage(ctx context.Context, opts DownloadOpts) (string, error) {
	if opts.Path != "" {
		if _, err := os.Stat(opts.Path); err != nil {
			return "", errors.Wrapf(err, "disk image %s", opts.Path)
		}
		return opts.Path, nil
	}

	version := opts.Version
	if version == "" {
		version = os.Getenv("KVARN_IMAGE_VERSION")
	}
	explicitVersion := version != ""
	if version == "" {
		version = buildinfo.Version
	}

	arch := opts.Arch
	if arch == "" {
		arch = runtime.GOARCH
	}

	if !opts.ForceDownload {
		if path, _ := localDiskImagePath(); path != "" {
			return path, nil
		}

		cached, err := cachedDiskImagePath(version, arch)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(cached); err == nil {
			return cached, nil
		}

		if opts.NoDownload {
			return "", errors.Newf(
				"no local or cached VM disk image found for %s (version %q); "+
					"omit --no-download to fetch it",
				arch,
				version,
			)
		}
	}

	if opts.ForceDownload || explicitVersion || buildinfo.IsRelease(version) {
		if !explicitVersion && !buildinfo.IsRelease(version) {
			// e.g. `kvarn image pull` on a dev build with no version set.
			return "", errors.Newf(
				"cannot download an image for non-release version %q; "+
					"set KVARN_IMAGE_VERSION or pass --version vX.Y.Z",
				version,
			)
		}
		return downloadDiskImage(ctx, version, arch, opts.Progress)
	}

	return "", errors.Newf(
		"no VM disk image found for %s, and version %q has no published release to download.\n"+
			"Build one locally with `task image:build`, or pass --disk-image-path to point at an existing image.",
		arch,
		version,
	)
}
