//go:build darwin

package scheduler

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// hostMemoryBytes returns total physical memory in bytes via sysctl hw.memsize.
//
// The value must be read with the uint64 accessor rather than syscall.Sysctl:
// that one returns the raw bytes as a Go string and strips a trailing NUL, which
// for a little-endian integer is an ordinary zero high byte — every plausible
// memory size has one, so the 8-byte value would always arrive truncated.
func hostMemoryBytes() (uint64, error) {
	v, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, fmt.Errorf("sysctl hw.memsize: %w", err)
	}
	return v, nil
}
