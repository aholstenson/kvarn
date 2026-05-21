package runner

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
)

func gatherVmInfo() *v1.VmInfo {
	info := &v1.VmInfo{}

	if f, err := os.Open("/proc/cpuinfo"); err == nil {
		defer f.Close()
		s := bufio.NewScanner(f)
		for s.Scan() {
			line := s.Text()
			if k, v, ok := strings.Cut(line, ":"); ok {
				k = strings.TrimSpace(k)
				v = strings.TrimSpace(v)
				switch k {
				case "processor":
					info.CpuCount++
				case "model name":
					if info.CpuModel == "" {
						info.CpuModel = v
					}
				}
			}
		}
	}

	if f, err := os.Open("/proc/meminfo"); err == nil {
		defer f.Close()
		s := bufio.NewScanner(f)
		for s.Scan() {
			line := s.Text()
			if k, v, ok := strings.Cut(line, ":"); ok {
				k = strings.TrimSpace(k)
				v = strings.TrimSpace(v)
				// Values are in kB, e.g. "1024000 kB"
				v = strings.TrimSuffix(v, " kB")
				switch k {
				case "MemTotal":
					if kb, err := strconv.ParseInt(v, 10, 64); err == nil {
						info.MemTotalMb = kb / 1024
					}
				case "MemAvailable":
					if kb, err := strconv.ParseInt(v, 10, 64); err == nil {
						info.MemAvailableMb = kb / 1024
					}
				}
			}
		}
	}

	// df -m / outputs: Filesystem 1M-blocks Used Available Use% Mounted on
	if out, err := exec.Command("df", "-m", "/").Output(); err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) >= 2 {
			fields := strings.Fields(lines[1])
			if len(fields) >= 4 {
				if total, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					info.DiskTotalMb = total
				}
				if used, err := strconv.ParseInt(fields[2], 10, 64); err == nil {
					info.DiskUsedMb = used
				}
			}
		}
	}

	return info
}
