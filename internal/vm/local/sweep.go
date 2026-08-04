package local

import (
	"log/slog"
	"os"
	"path/filepath"
)

// vmTempGlobs match every temp file a local VM creates. The disk file is named
// from a fixed prefix and the seed ISO, QMP socket and NVRAM store are all
// derived from it by suffix, so two patterns cover all of them.
var vmTempGlobs = []string{
	"kvarn-disk-*",
	"kvarn-ovmf-vars-*",
}

// sweepStaleVMFiles removes VM temp files left in dir by a previous
// orchestrator. It is called once at provider construction, before this process
// has created a VM, so everything it matches belongs to a run that is over —
// which is what makes a blind prefix sweep safe here and nowhere else.
//
// It matters more than it looks. A crash mid-boot leaves a VM's disk behind:
// on Linux a qcow2 overlay, but on macOS a full raw copy of the base image,
// resized to the job's disk request. Those land on the same filesystem the
// admission pool is sized from, so without this a host that restarts under
// load quietly loses the space it needs to admit the jobs it just requeued.
//
// Like the QEMU reaping beside it, this assumes one orchestrator per host —
// the same assumption the session database and the VM table already make.
func sweepStaleVMFiles(dir string) {
	removed := 0
	var bytes int64
	for _, pattern := range vmTempGlobs {
		matches, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			// The patterns are constants, so this cannot fire in practice.
			slog.Warn("vm temp sweep pattern failed", "pattern", pattern, "error", err)
			continue
		}
		for _, path := range matches {
			if info, err := os.Stat(path); err == nil {
				bytes += info.Size()
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				slog.Warn("could not remove stale vm temp file", "path", path, "error", err)
				continue
			}
			removed++
		}
	}
	if removed > 0 {
		slog.Info("swept stale VM temp files from a previous run", "count", removed, "bytes", bytes)
	}
}
