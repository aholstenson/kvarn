// Package linebuf accumulates byte-aligned chunks of streamed output and
// re-emits them as complete lines.
package linebuf

import "strings"

// Buffer accumulates byte chunks and emits complete lines (without the
// trailing newline). Partial trailing bytes are held until the next Append or
// Flush so a chunk that arrives mid-line does not produce a spurious line.
type Buffer struct{ tail string }

// Append adds chunk to the buffer and returns any complete lines made
// available by it. A trailing \r is stripped from each returned line so
// \r\n-terminated input renders cleanly.
func (b *Buffer) Append(chunk string) []string {
	if chunk == "" {
		return nil
	}
	if b.tail != "" {
		chunk = b.tail + chunk
		b.tail = ""
	}
	if !strings.Contains(chunk, "\n") {
		b.tail = chunk
		return nil
	}
	parts := strings.Split(chunk, "\n")
	b.tail = parts[len(parts)-1]
	parts = parts[:len(parts)-1]
	for i, p := range parts {
		if len(p) > 0 && p[len(p)-1] == '\r' {
			parts[i] = p[:len(p)-1]
		}
	}
	return parts
}

// Flush returns any held partial-line tail and clears it. Returns "" if no
// tail is held.
func (b *Buffer) Flush() string {
	t := b.tail
	b.tail = ""
	if len(t) > 0 && t[len(t)-1] == '\r' {
		t = t[:len(t)-1]
	}
	return t
}
