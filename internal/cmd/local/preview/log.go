package preview

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/aholstenson/kvarn/internal/linebuf"
)

// serviceLog collects what the preview's servers print.
//
// It has two lives. While the preview is coming up the task UI owns the
// terminal, so output is held: a server's startup chatter is only wanted if
// something fails, and interleaving it with the spinner would corrupt both.
// Once everything is ready the terminal is handed over — the buffer is replayed
// and later output goes straight through, prefixed with the service that wrote
// it, which is what `kvarn local preview` looks like for the rest of its run.
type serviceLog struct {
	mu   sync.Mutex
	held strings.Builder
	out  io.Writer
	// lines splits each service's output into whole lines, so a prefix is never
	// written into the middle of one.
	lines map[string]*linebuf.Buffer
}

func newServiceLog() *serviceLog {
	return &serviceLog{lines: map[string]*linebuf.Buffer{}}
}

// write records output from a service.
func (l *serviceLog) write(name, s string) {
	if s == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	buf, ok := l.lines[name]
	if !ok {
		buf = &linebuf.Buffer{}
		l.lines[name] = buf
	}
	for _, line := range buf.Append(s) {
		l.emit(fmt.Sprintf("%s | %s\n", name, line))
	}
}

// note records a line kvarn itself is saying about the services, rather than
// one of them saying it.
func (l *serviceLog) note(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.emit("==> " + msg + "\n")
}

// emit writes one complete line, either to the holding buffer or to the
// terminal. The caller holds the lock.
func (l *serviceLog) emit(line string) {
	if l.out != nil {
		fmt.Fprint(l.out, line)
		return
	}
	l.held.WriteString(line)
}

// streaming reports whether output is going to the terminal already.
func (l *serviceLog) streaming() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.out != nil
}

// streamTo replays what was held and sends everything after it to w.
func (l *serviceLog) streamTo(w io.Writer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if held := l.held.String(); held != "" {
		fmt.Fprint(w, held)
		l.held.Reset()
	}
	l.out = w
}

// dump writes what was held without taking over the stream. It is the failure
// path: the preview never came up, and what the servers printed on the way is
// the explanation.
func (l *serviceLog) dump(w io.Writer) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Flush partial trailing lines too — a server that died mid-line is exactly
	// the case this is being read for.
	for name, buf := range l.lines {
		if tail := buf.Flush(); tail != "" {
			l.held.WriteString(fmt.Sprintf("%s | %s\n", name, tail))
		}
	}
	if held := l.held.String(); held != "" {
		fmt.Fprintf(w, "\n--- services ---\n%s", held)
		l.held.Reset()
	}
}
