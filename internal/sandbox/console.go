package sandbox

import (
	"strings"
	"sync"
)

// consoleTailLines is how many serial console lines are kept.
//
// What explains a runner that stopped answering is the end of the console, not
// the beginning: an OOM kill, a kernel panic, a filesystem remounted read-only.
// A fixed tail keeps that end available for the whole life of a session without
// the memory growing with a stream nobody reads until something breaks.
const consoleTailLines = 50

// ConsoleTail holds the most recent serial console lines from a guest.
//
// The console is the only channel a dying guest still has: the bridge reports
// the same silence whether the runner crashed, the kernel killed it, or the VM
// itself went away. Attaching the tail to the error a lost bridge produces is
// what turns "runner disconnected" into a reason.
type ConsoleTail struct {
	mu    sync.Mutex
	lines []string
}

// NewConsoleTail returns an empty ConsoleTail.
func NewConsoleTail() *ConsoleTail {
	return &ConsoleTail{}
}

// Add records a chunk of console output, dropping the oldest lines once the
// tail is full. A chunk is normally one line, but it is split either way so a
// provider that batches them does not push several lines into one slot and
// shorten the history that many lines.
func (c *ConsoleTail) Add(output string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		c.lines = append(c.lines, line)
	}
	if excess := len(c.lines) - consoleTailLines; excess > 0 {
		c.lines = append(c.lines[:0], c.lines[excess:]...)
	}
}

// Lines returns the retained lines, oldest first.
func (c *ConsoleTail) Lines() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

// String renders the retained lines one per line, and is empty when the guest
// has said nothing.
func (c *ConsoleTail) String() string {
	return strings.Join(c.Lines(), "\n")
}
