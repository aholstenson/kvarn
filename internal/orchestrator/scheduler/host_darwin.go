//go:build darwin

package scheduler

import (
	"encoding/binary"
	"fmt"
	"syscall"
)

// hostMemoryBytes returns total physical memory in bytes via sysctl hw.memsize,
// which kernel ABI promises is a uint64 little-endian value.
func hostMemoryBytes() (uint64, error) {
	raw, err := syscall.Sysctl("hw.memsize")
	if err != nil {
		return 0, fmt.Errorf("sysctl hw.memsize: %w", err)
	}
	// syscall.Sysctl strips a trailing NUL; the value is 8 raw bytes.
	if len(raw) < 8 {
		return 0, fmt.Errorf("sysctl hw.memsize: short read (%d bytes)", len(raw))
	}
	return binary.LittleEndian.Uint64([]byte(raw)[:8]), nil
}
