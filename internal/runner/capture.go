package runner

import (
	"fmt"
	"os"
)

// capBuffer collects a command's output while holding at most limit bytes in
// memory: the first half of the budget from the start of the stream and the
// last half from its end.
//
// Both ends are kept because both ends carry meaning. A build or test run
// names what it is doing at the top and reports what went wrong at the bottom,
// so a head-only cap throws away the answer and a tail-only cap throws away
// the question. What sits between them — thousands of passing test lines, a
// dependency tree, a binary file grep walked into — is the part nobody needs.
//
// A limit of 0 disables capping and the buffer keeps everything.
type capBuffer struct {
	headCap int
	tailCap int
	head    []byte
	tail    []byte
	total   int64
}

// newCapBuffer returns a buffer retaining at most limit bytes. limit <= 0
// retains everything.
func newCapBuffer(limit int) *capBuffer {
	if limit <= 0 {
		return &capBuffer{}
	}
	head := limit / 2
	return &capBuffer{headCap: head, tailCap: limit - head}
}

func (b *capBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.total += int64(n)

	if b.headCap == 0 && b.tailCap == 0 {
		b.head = append(b.head, p...)
		return n, nil
	}

	if len(b.head) < b.headCap {
		take := min(b.headCap-len(b.head), len(p))
		b.head = append(b.head, p[:take]...)
		p = p[take:]
	}
	if len(p) == 0 {
		return n, nil
	}

	// Everything past the head budget rolls through the tail, which keeps only
	// the most recent tailCap bytes.
	if len(p) >= b.tailCap {
		b.tail = append(b.tail[:0], p[len(p)-b.tailCap:]...)
		return n, nil
	}
	if drop := len(b.tail) + len(p) - b.tailCap; drop > 0 {
		b.tail = append(b.tail[:0], b.tail[drop:]...)
	}
	b.tail = append(b.tail, p...)
	return n, nil
}

// Total is the number of bytes written, including what was dropped.
func (b *capBuffer) Total() int64 { return b.total }

// Truncated reports whether anything was dropped.
func (b *capBuffer) Truncated() bool {
	return b.total > int64(len(b.head))+int64(len(b.tail))
}

// String returns the retained output. When bytes were dropped the two retained
// ends are joined by a marker naming how much went missing, so every reader of
// the output — the model, the live log, a retry prompt — sees that it is
// looking at an excerpt without having to consult the byte counts alongside it.
func (b *capBuffer) String() string {
	if !b.Truncated() {
		return string(b.head) + string(b.tail)
	}
	dropped := b.total - int64(len(b.head)) - int64(len(b.tail))
	return string(b.head) + truncationMarker(dropped) + string(b.tail)
}

// truncationMarker is the line spliced in where output was dropped.
func truncationMarker(dropped int64) string {
	return fmt.Sprintf("\n…[kvarn: %s of output omitted]…\n", formatBytes(dropped))
}

// readCappedFile reads a demarcation output file under the same head/tail rule
// as capBuffer, seeking to the two ends rather than loading the file. A command
// that wrote gigabytes to stdout must not be able to make the runner read them
// all back into memory just to answer with an excerpt.
//
// It reports the retained text, the file's true size, and whether anything was
// dropped. A missing or unreadable file reads as empty, matching the previous
// behavior of ignoring the error: the exit status is the authority on whether
// the command ran.
func readCappedFile(path string, limit int) (text string, total int64, truncated bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", 0, false
	}
	size := info.Size()

	if limit <= 0 || size <= int64(limit) {
		buf := make([]byte, size)
		n, _ := f.ReadAt(buf, 0)
		return string(buf[:n]), size, false
	}

	headCap := limit / 2
	tailCap := limit - headCap

	head := make([]byte, headCap)
	hn, _ := f.ReadAt(head, 0)

	tail := make([]byte, tailCap)
	tn, _ := f.ReadAt(tail, size-int64(tailCap))

	dropped := size - int64(hn) - int64(tn)
	if dropped <= 0 {
		return string(head[:hn]) + string(tail[:tn]), size, false
	}
	return string(head[:hn]) + truncationMarker(dropped) + string(tail[:tn]), size, true
}

// formatBytes renders a byte count for the truncation marker.
func formatBytes(b int64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case b >= gib:
		return fmt.Sprintf("%.1fG", float64(b)/float64(gib))
	case b >= mib:
		return fmt.Sprintf("%.1fM", float64(b)/float64(mib))
	case b >= kib:
		return fmt.Sprintf("%.1fK", float64(b)/float64(kib))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
