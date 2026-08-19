package preview

import (
	"strings"
	"sync"
)

// DefaultLogCapacity is how much of a preview's output is kept in memory.
//
// A server running for hours produces unbounded output, so full persistence is
// not on the table: the session event log caps a single payload at 256 KiB and
// a SQLite file that grows with every request logged would be a liability
// rather than a feature. What is worth keeping is the recent tail — enough to
// see why something is broken right now — and only start, exit and failure go
// to the durable event log.
const DefaultLogCapacity = 256 * 1024

// LogBuffer keeps the last N bytes of a preview's process output. Writes are
// cheap and never block, because they happen on the bridge's output-dispatch
// goroutine.
type LogBuffer struct {
	mu       sync.Mutex
	capacity int
	buf      []byte
	// dropped counts bytes discarded to stay within capacity, so a reader can
	// be told the view is partial rather than assuming it saw the whole run.
	dropped int64
}

// NewLogBuffer creates a buffer holding at most capacity bytes. A capacity of
// zero or less takes DefaultLogCapacity.
func NewLogBuffer(capacity int) *LogBuffer {
	if capacity <= 0 {
		capacity = DefaultLogCapacity
	}
	return &LogBuffer{capacity: capacity}
}

// Append adds output to the buffer, discarding the oldest bytes to stay within
// capacity.
func (b *LogBuffer) Append(s string) {
	if s == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	b.buf = append(b.buf, s...)
	if excess := len(b.buf) - b.capacity; excess > 0 {
		b.dropped += int64(excess)
		// Copy the tail down rather than resliced-in-place, so the discarded
		// prefix is actually released instead of pinned by the slice header.
		b.buf = append(b.buf[:0], b.buf[excess:]...)
	}
}

// String returns the retained output.
func (b *LogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// Tail returns the last n lines of retained output. Fewer are returned when
// that is all there is; n <= 0 returns everything retained.
func (b *LogBuffer) Tail(n int) string {
	text := b.String()
	if n <= 0 || text == "" {
		return text
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// Dropped returns how many bytes have been discarded to stay within capacity.
func (b *LogBuffer) Dropped() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// Reset empties the buffer, for a preview that is booting again.
func (b *LogBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = b.buf[:0]
	b.dropped = 0
}
