package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/aholstenson/kvarn/internal/buildinfo"
	"github.com/cockroachdb/errors"
)

// cachedImageName is the filename used for a downloaded disk image inside its
// per-version, per-arch cache directory.
const cachedImageName = "disk.qcow2"

// downloadWaitTimeout bounds how long a process waits for another process's
// in-flight download before giving up.
const downloadWaitTimeout = 10 * time.Minute

// releaseBaseURL is the base URL for release asset downloads. It is a package
// var so tests can point it at an httptest server.
var releaseBaseURL = "https://github.com/" + buildinfo.Repo + "/releases/download"

// imageIndexTag is the perpetual release that hosts the image manifest. It is
// clobbered on every image release, so a fixed URL avoids a per-boot GitHub
// API call to discover available images.
const imageIndexTag = "image-index"

// imageManifestName is the manifest asset listing every published image
// version and the arches it was built for.
const imageManifestName = "images.json"

// userCacheDir locates the user cache root. It is a var so tests can redirect
// the image cache to a temp dir.
var userCacheDir = os.UserCacheDir

// imageCacheDir returns the per-version, per-arch cache directory for the disk
// image, mirroring cache.DefaultFileCache's ~/.cache/kvarn root.
func imageCacheDir(version, arch string) (string, error) {
	dir, err := userCacheDir()
	if err != nil {
		return "", errors.Wrap(err, "determine user cache dir")
	}
	return filepath.Join(dir, "kvarn", "images", version, arch), nil
}

// cachedDiskImagePath returns the path to the cached disk image file for the
// given version and arch, whether or not it exists yet.
func cachedDiskImagePath(version, arch string) (string, error) {
	dir, err := imageCacheDir(version, arch)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, cachedImageName), nil
}

// downloadDiskImage fetches the disk image for version/arch into the user
// cache, verifies it against the published .sha256, and returns the resolved
// path. The download is checksum-verified and written atomically, so a partial
// or corrupt fetch never leaves a usable file behind.
//
// progress, if non-nil, is called as bytes arrive with the number of bytes
// downloaded so far and the total (or -1 if the server omits Content-Length).
func downloadDiskImage(ctx context.Context, version, arch string, progress func(done, total int64)) (string, error) {
	dir, err := imageCacheDir(version, arch)
	if err != nil {
		return "", err
	}
	dest := filepath.Join(dir, cachedImageName)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", errors.Wrapf(err, "create image cache dir %s", dir)
	}

	// Best-effort cross-process lock so two kvarn invocations don't both pull
	// ~450 MB. If we can't take the lock another process is downloading; wait
	// for it and reuse the result.
	lockPath := filepath.Join(dir, ".download.lock")
	locked, unlock := acquireDownloadLock(lockPath)
	if !locked {
		return waitForDownload(ctx, dest, lockPath)
	}
	defer unlock()

	// Recheck the cache now that we hold the lock — another process may have
	// finished between our cache miss and acquiring the lock.
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}

	assetURL := fmt.Sprintf("%s/%s/kvarn-disk-%s.qcow2", releaseBaseURL, imageReleaseTag(version), arch)
	wantSum, err := fetchChecksum(ctx, assetURL+".sha256")
	if err != nil {
		return "", err
	}

	tmp, err := os.CreateTemp(dir, ".disk-*.qcow2.tmp")
	if err != nil {
		return "", errors.Wrap(err, "create temp file for image")
	}
	tmpName := tmp.Name()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", errors.Wrap(err, "build image request")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", errors.Wrapf(err, "download image %s", assetURL)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		tmp.Close()
		os.Remove(tmpName)
		return "", errors.Newf("download image %s: unexpected status %s", assetURL, resp.Status)
	}

	// Hash while streaming so we never read the file back. The counting writer
	// drives the progress callback off Content-Length.
	hasher := sha256.New()
	w := io.MultiWriter(tmp, hasher)
	if progress != nil {
		w = io.MultiWriter(tmp, hasher, &countingWriter{total: resp.ContentLength, progress: progress})
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", errors.Wrap(err, "write image")
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", errors.Wrap(err, "close image temp file")
	}

	gotSum := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(gotSum, wantSum) {
		os.Remove(tmpName)
		return "", errors.Newf(
			"image checksum mismatch for %s:\n  want %s\n  got  %s",
			assetURL, wantSum, gotSum,
		)
	}

	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return "", errors.Wrapf(err, "move image into place %s", dest)
	}
	return dest, nil
}

// fetchChecksum downloads a sha256sum-style file and returns the hex digest
// (the first whitespace-separated field).
func fetchChecksum(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", errors.Wrap(err, "build checksum request")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", errors.Wrapf(err, "download checksum %s", url)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.Newf("download checksum %s: unexpected status %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", errors.Wrapf(err, "read checksum %s", url)
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return "", errors.Newf("checksum %s is empty", url)
	}
	return fields[0], nil
}

// acquireDownloadLock attempts to create lockPath exclusively. It returns
// (true, unlock) when this process now holds the lock, or (false, nil) when the
// lock is already held. Acquisition is best-effort: any error other than
// "already exists" is treated as not-held so an unwritable lock never blocks a
// download outright (the worst case is a redundant parallel fetch).
func acquireDownloadLock(lockPath string) (bool, func()) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return true, func() {}
	}
	f.Close()
	return true, func() { os.Remove(lockPath) }
}

// waitForDownload polls for an in-flight download by another process to
// produce dest, or for its lock to disappear. A vanished lock without a file
// means the other process failed; a generous timeout guards against a stale
// lock left by a crashed process.
func waitForDownload(ctx context.Context, dest, lockPath string) (string, error) {
	deadline := time.Now().Add(downloadWaitTimeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(dest); err == nil {
			return dest, nil
		}
		if _, err := os.Stat(lockPath); errors.Is(err, os.ErrNotExist) {
			if _, err := os.Stat(dest); err == nil {
				return dest, nil
			}
			return "", errors.New("concurrent image download finished without producing the image; retry")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return "", errors.Newf(
					"timed out waiting for a concurrent image download; remove %s if it is stale",
					lockPath,
				)
			}
		}
	}
}

// imageReleaseTag maps a concrete image version to its GitHub release tag.
// Images are released independently of the CLI under image-v<X.Y.Z> tags, so
// the version is normalized (any leading "v" stripped) before re-prefixing.
func imageReleaseTag(version string) string {
	return "image-v" + strings.TrimPrefix(version, "v")
}

// imageManifest is the schema of the images.json asset on the image-index
// release.
type imageManifest struct {
	Images []imageManifestEntry `json:"images"`
}

type imageManifestEntry struct {
	Version string   `json:"version"`
	Arches  []string `json:"arches"`
}

// fetchImageManifest downloads and parses images.json from the image-index
// release.
func fetchImageManifest(ctx context.Context) (*imageManifest, error) {
	url := fmt.Sprintf("%s/%s/%s", releaseBaseURL, imageIndexTag, imageManifestName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.Wrap(err, "build image manifest request")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, errors.Wrapf(err, "download image manifest %s", url)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.Newf("download image manifest %s: unexpected status %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, errors.Wrapf(err, "read image manifest %s", url)
	}
	var m imageManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, errors.Wrapf(err, "parse image manifest %s", url)
	}
	return &m, nil
}

// resolveImageVersion fetches the published image manifest and returns the
// highest concrete version satisfying cs that was built for arch.
func resolveImageVersion(ctx context.Context, cs *semver.Constraints, arch string) (string, error) {
	m, err := fetchImageManifest(ctx)
	if err != nil {
		return "", err
	}
	var best *semver.Version
	for _, img := range m.Images {
		if !slices.Contains(img.Arches, arch) {
			continue
		}
		v, err := semver.NewVersion(img.Version)
		if err != nil {
			continue
		}
		if !cs.Check(v) {
			continue
		}
		if best == nil || v.GreaterThan(best) {
			best = v
		}
	}
	if best == nil {
		return "", errors.Newf("no published image satisfies %q for %s", cs.String(), arch)
	}
	return best.String(), nil
}

// countingWriter reports cumulative byte counts to a progress callback.
type countingWriter struct {
	done     int64
	total    int64
	progress func(done, total int64)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.done += int64(len(p))
	c.progress(c.done, c.total)
	return len(p), nil
}
