package vm

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// searchPaths are well-known locations to look for the disk image, in order.
// The first match wins. Each path is joined with the image subpath
// (e.g. "arm64/disk.img").
var searchPaths = []string{
	"/usr/local/share/kvarn/dist",
	"/opt/kvarn/dist",
}

// ResolveDiskImagePath finds the disk image for the current architecture.
// It checks the following locations in order:
//  1. Relative to the running binary (e.g. <binary-dir>/dist/<arch>/disk.img)
//  2. Well-known system paths
//
// Returns the resolved absolute path, or an error listing all locations that
// were checked.
func ResolveDiskImagePath() (string, error) {
	sub := filepath.Join(runtime.GOARCH, "disk.qcow2")

	var checked []string

	// 1. Relative to the binary.
	if execPath, err := os.Executable(); err == nil {
		binDir := filepath.Dir(execPath)
		candidate := filepath.Join(binDir, "dist", sub)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		checked = append(checked, candidate)
	}

	// 2. Well-known system paths.
	for _, dir := range searchPaths {
		candidate := filepath.Join(dir, sub)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		checked = append(checked, candidate)
	}

	return "", fmt.Errorf(
		"could not find disk image %q in any of:\n  %s",
		sub,
		strings.Join(checked, "\n  "),
	)
}
