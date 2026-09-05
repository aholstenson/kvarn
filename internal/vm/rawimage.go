package vm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/aholstenson/kvarn/internal/vm/disk"
)

// convertToRaw is a var so tests can stand in for a real qcow2 conversion.
var convertToRaw = disk.ConvertQcow2ToRaw

// rawImageMu serializes conversions in this process. A conversion reads and
// writes the whole image, so two VMs that start together must not both do it:
// the second waits here and then finds the cache warm.
var rawImageMu sync.Mutex

// rawDiskImageDir returns the cache directory that holds raw copies of disk
// images, beside the per-version image cache.
func rawDiskImageDir() (string, error) {
	dir, err := userCacheDir()
	if err != nil {
		return "", fmt.Errorf("determine user cache dir: %w", err)
	}
	return filepath.Join(dir, "kvarn", "images", "raw"), nil
}

// rawImageKey identifies the source image a raw copy was made from. The path
// hash separates images, and the size and modification time invalidate the copy
// when the source is rebuilt in place, which is what `task image:build` does to
// dist/<arch>/disk.qcow2.
func rawImageKey(src string, info os.FileInfo) (prefix, name string) {
	abs, err := filepath.Abs(src)
	if err != nil {
		abs = src
	}
	sum := sha256.Sum256([]byte(abs))
	prefix = hex.EncodeToString(sum[:8])
	return prefix, fmt.Sprintf("%s-%d-%d.img", prefix, info.Size(), info.ModTime().UnixNano())
}

// EnsureRawDiskImage returns the path of a raw copy of the qcow2 image at src,
// converting it once into the user cache and reusing it afterwards.
//
// A provider that hands each VM a copy-on-write clone of its disk needs a raw
// source to clone from, because a clone is a byte-for-byte copy and the guest
// boots from a raw device. Converting once per image instead of once per VM is
// what makes a VM start in the time a clone takes.
//
// The copy is sparse, so it costs the image's used space rather than its
// virtual size. Raw copies of earlier revisions of the same source are pruned
// once the new one is in place.
func EnsureRawDiskImage(src string) (string, error) {
	info, err := os.Stat(src)
	if err != nil {
		return "", fmt.Errorf("stat disk image %s: %w", src, err)
	}

	dir, err := rawDiskImageDir()
	if err != nil {
		return "", err
	}
	prefix, name := rawImageKey(src, info)
	dest := filepath.Join(dir, name)

	rawImageMu.Lock()
	defer rawImageMu.Unlock()

	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create raw image cache dir %s: %w", dir, err)
	}

	// Convert into a temp file and rename, so a crash mid-conversion never
	// leaves a truncated image that a later run would boot from.
	tmp, err := os.CreateTemp(dir, ".raw-*.img.tmp")
	if err != nil {
		return "", fmt.Errorf("create temp file for raw image: %w", err)
	}
	tmpName := tmp.Name()
	tmp.Close()

	if err := convertToRaw(src, tmpName); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("convert %s to raw: %w", src, err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("move raw image into place %s: %w", dest, err)
	}

	pruneRawDiskImages(dir, prefix, name)
	return dest, nil
}

// pruneRawDiskImages removes raw copies of older revisions of one source image.
// Removing a copy cannot disturb a running VM: a clone owns its data from the
// moment it is made, so it outlives the file it was cloned from.
func pruneRawDiskImages(dir, prefix, keep string) {
	matches, err := filepath.Glob(filepath.Join(dir, prefix+"-*.img"))
	if err != nil {
		// The pattern is built from a hex prefix, so this cannot fire.
		return
	}
	for _, path := range matches {
		if filepath.Base(path) == keep {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("could not remove stale raw disk image", "path", path, "error", err)
		}
	}
}
