// Package imageutil holds small helpers shared by CLI commands that resolve or
// download the VM disk image.
package imageutil

import (
	"fmt"
	"io"
)

// NewProgress returns a vm.DownloadOpts.Progress callback that writes a single,
// rewritten status line to w. The label is printed once, on the first call, so
// nothing is emitted unless a download actually starts. A trailing newline is
// written when the total is known and reached.
func NewProgress(w io.Writer, label string) func(done, total int64) {
	started := false
	return func(done, total int64) {
		if !started {
			started = true
			fmt.Fprintln(w, label)
		}
		if total > 0 {
			fmt.Fprintf(w, "\r  %.0f%% (%.1f / %.1f MB)",
				float64(done)/float64(total)*100,
				float64(done)/(1024*1024),
				float64(total)/(1024*1024))
			if done >= total {
				fmt.Fprintln(w)
			}
		} else {
			fmt.Fprintf(w, "\r  %.1f MB", float64(done)/(1024*1024))
		}
	}
}
