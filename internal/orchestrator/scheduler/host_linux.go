//go:build linux

package scheduler

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// hostMemoryBytes reads MemTotal from /proc/meminfo. The value is reported in
// kibibytes; we convert to bytes here so callers see one consistent unit.
func hostMemoryBytes() (uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("open /proc/meminfo: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("/proc/meminfo: malformed MemTotal line %q", line)
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("/proc/meminfo: parse MemTotal %q: %w", fields[1], err)
		}
		return kb * 1024, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	return 0, fmt.Errorf("/proc/meminfo: MemTotal not found")
}
