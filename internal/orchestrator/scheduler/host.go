package scheduler

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

// defaultHostFraction is the share of memory and free disk space the scheduler
// claims by default — leaving the rest for the kernel, the orchestrator process
// itself, and other host workloads.
const defaultHostFraction = 0.75

// HostCPUMillis returns the host vCPU count scaled to millicpus.
func HostCPUMillis() uint64 {
	return uint64(runtime.NumCPU()) * 1000
}

// HostMemoryBytes returns the platform-detected total physical memory.
func HostMemoryBytes() (uint64, error) {
	return hostMemoryBytes()
}

// HostFreeDiskBytes returns bytes available to non-root users on the filesystem
// hosting path. path is created if missing so the statfs call is meaningful on
// a fresh install.
func HostFreeDiskBytes(path string) (uint64, error) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return 0, fmt.Errorf("ensure %s: %w", path, err)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	// Bavail is reserved for unprivileged users — closer to what we can
	// actually allocate than Bfree.
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// DefaultImageCacheDir returns the same directory vm.imageCacheDir would, minus
// the per-version/per-arch suffix — i.e. the parent that grows as images
// accumulate. Used by the scheduler to bound disk against the filesystem the VM
// images live on.
func DefaultImageCacheDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("determine user cache dir: %w", err)
	}
	return filepath.Join(dir, "kvarn", "images"), nil
}

// FractionOf returns floor(v * defaultHostFraction). Centralised so tests and
// the CLI agree on the default scaling.
func FractionOf(v uint64) uint64 {
	return uint64(float64(v) * defaultHostFraction)
}
