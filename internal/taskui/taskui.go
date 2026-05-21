package taskui

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

// ansiRE matches ANSI escape sequences (CSI, OSC, and single-char escapes).
var ansiRE = regexp.MustCompile(`\x1b(?:\[[0-9;]*[a-zA-Z]|\][^\x07]*\x07|[()][A-B0-2])`)

// stripANSI removes ANSI escape sequences and handles carriage returns
// so that command output doesn't interfere with the renderer's own cursor
// management.
func stripANSI(s string) string {
	s = ansiRE.ReplaceAllString(s, "")
	// Handle carriage return: keep only the text after the last \r.
	if i := strings.LastIndex(s, "\r"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// visibleLen returns the number of runes in s after stripping ANSI sequences.
func visibleLen(s string) int {
	return utf8.RuneCountInString(ansiRE.ReplaceAllString(s, ""))
}

// truncateVisible truncates s so its visible width does not exceed max runes,
// appending "…" when truncated.
func truncateVisible(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max > 1 {
		return string(runes[:max-1]) + "…"
	}
	return string(runes[:max])
}

// Status represents the current state of a task item.
type Status int

const (
	StatusPending Status = iota
	StatusRunning
	StatusPassed
	StatusFailed
	StatusSkipped
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Item represents a single task in the renderer.
type Item struct {
	Name     string
	Status   Status
	Suffix   string
	Output   []string
	Children []*Item
}

// entry is either a section header or an item pointer.
type entry struct {
	section string // non-empty for section headers
	item    *Item
}

// Renderer draws a live task list to a terminal.
type Renderer struct {
	mu         sync.Mutex
	w          io.Writer
	isTTY      bool
	verbose    bool
	liveLines  int
	bufferLines int
	termWidth  int
	entries    []entry
	drawnLines int
	stopCh   chan struct{}
	stopOnce sync.Once
	spinIdx  int
}

// New creates a renderer. It detects whether w is a TTY (when w is os.Stdout
// or os.Stderr). When verbose is true, all output lines are kept; otherwise
// the buffer is capped so failed items can replay the last bufferLines lines
// at the end, while the live redraw region is bounded to liveLines.
func New(w io.Writer, verbose bool) *Renderer {
	isTTY := false
	termWidth := 0
	if f, ok := w.(*os.File); ok {
		isTTY = term.IsTerminal(int(f.Fd()))
		if isTTY {
			if w, _, err := term.GetSize(int(f.Fd())); err == nil {
				termWidth = w
			}
		}
	}
	return &Renderer{
		w:           w,
		isTTY:       isTTY,
		verbose:     verbose,
		liveLines:   20,
		bufferLines: 200,
		termWidth:   termWidth,
		stopCh:      make(chan struct{}),
	}
}

// Start begins the spinner animation (TTY mode only).
func (r *Renderer) Start() {
	if !r.isTTY {
		return
	}
	// Hide cursor.
	fmt.Fprint(r.w, "\033[?25l")
	go r.spin()
}

// Stop ends the spinner animation and shows the cursor.
func (r *Renderer) Stop() {
	if !r.isTTY {
		return
	}
	r.stopOnce.Do(func() {
		close(r.stopCh)
		r.mu.Lock()
		defer r.mu.Unlock()
		// Clear active region and do a final render.
		r.clearActive()
		r.renderFinal()
		// Show cursor.
		fmt.Fprint(r.w, "\033[?25h")
	})
}

func (r *Renderer) spin() {
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.mu.Lock()
			r.spinIdx = (r.spinIdx + 1) % len(spinnerFrames)
			r.render()
			r.mu.Unlock()
		}
	}
}

// AddSection adds a section header (e.g. "Setup").
func (r *Renderer) AddSection(title string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, entry{section: title})
	if !r.isTTY {
		fmt.Fprintf(r.w, "\n=== %s ===\n", title)
	}
}

// AddItem adds a top-level task item and returns it.
func (r *Renderer) AddItem(name string) *Item {
	r.mu.Lock()
	defer r.mu.Unlock()
	item := &Item{Name: name, Status: StatusPending}
	r.entries = append(r.entries, entry{item: item})
	return item
}

// AddChild adds a child item under a parent.
func (r *Renderer) AddChild(parent *Item, name string) *Item {
	r.mu.Lock()
	defer r.mu.Unlock()
	child := &Item{Name: name, Status: StatusRunning}
	parent.Children = append(parent.Children, child)
	return child
}

// SetStatus updates an item's status and suffix.
func (r *Renderer) SetStatus(item *Item, status Status, suffix string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item.Status = status
	item.Suffix = suffix
	if !r.isTTY {
		icon := PlainIcon(status)
		line := fmt.Sprintf("%s %s", icon, item.Name)
		if suffix != "" {
			line += " " + suffix
		}
		fmt.Fprintln(r.w, line)
		// In plain mode, print buffered output for failed items.
		if status == StatusFailed && len(item.Output) > 0 {
			for _, ol := range item.Output {
				fmt.Fprintf(r.w, "  %s\n", ol)
			}
		}
	}
}

// AppendOutput adds an output line to an item's rolling buffer. ANSI escape
// sequences are stripped so that command output cannot interfere with the
// renderer's own cursor management.
func (r *Renderer) AppendOutput(item *Item, line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	line = stripANSI(line)
	item.Output = append(item.Output, line)
	if !r.verbose && len(item.Output) > r.bufferLines {
		item.Output = item.Output[len(item.Output)-r.bufferLines:]
	}
	if !r.isTTY && r.verbose {
		fmt.Fprintf(r.w, "  %s\n", line)
	}
}

// WriteRaw writes frozen text that won't be redrawn (e.g. VM info).
func (r *Renderer) WriteRaw(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.isTTY {
		r.clearActive()
		r.drawnLines = 0
	}
	fmt.Fprint(r.w, text)
}

// clearActive moves the cursor up and clears all drawn lines.
func (r *Renderer) clearActive() {
	if r.drawnLines > 0 {
		// Move up and clear each line.
		fmt.Fprintf(r.w, "\033[%dA", r.drawnLines)
		for range r.drawnLines {
			fmt.Fprint(r.w, "\033[2K\n")
		}
		fmt.Fprintf(r.w, "\033[%dA", r.drawnLines)
		r.drawnLines = 0
	}
}

// render redraws the active region (TTY only). Caller must hold r.mu.
func (r *Renderer) render() {
	r.clearActive()

	var buf strings.Builder
	lines := 0
	spinner := spinnerFrames[r.spinIdx]

	for _, e := range r.entries {
		if e.section != "" {
			buf.WriteString(fmt.Sprintf("\n\033[1m=== %s ===\033[0m\n", e.section))
			lines += 2
			continue
		}
		item := e.item
		l, c := r.renderItem(&buf, item, spinner, 0)
		lines += l
		_ = c
	}

	fmt.Fprint(r.w, buf.String())
	r.drawnLines = lines
}

// renderFinal writes the final state without spinners (TTY only). Caller must hold r.mu.
func (r *Renderer) renderFinal() {
	var buf strings.Builder
	for _, e := range r.entries {
		if e.section != "" {
			buf.WriteString(fmt.Sprintf("\n\033[1m=== %s ===\033[0m\n", e.section))
			continue
		}
		item := e.item
		r.renderItemFinal(&buf, item, 0)
	}
	fmt.Fprint(r.w, buf.String())
}

// renderItem renders a single item with spinner for TTY. Returns line count.
func (r *Renderer) renderItem(buf *strings.Builder, item *Item, spinner string, indent int) (int, int) {
	prefix := strings.Repeat("  ", indent)
	lines := 0

	icon := r.itemIcon(item, spinner)
	line := fmt.Sprintf("%s%s %s", prefix, icon, item.Name)
	if item.Suffix != "" {
		line += " \033[2m" + item.Suffix + "\033[0m"
	}
	buf.WriteString(line + "\n")
	lines += r.visualLines(line)

	// In the live render only show output for running or failed items,
	// capped to liveLines to keep the redraw zone bounded. The full
	// buffered output is shown by renderFinal when the renderer stops.
	showOutput := item.Status == StatusRunning || item.Status == StatusFailed
	if showOutput && len(item.Output) > 0 {
		output := item.Output
		if len(output) > r.liveLines {
			output = output[len(output)-r.liveLines:]
		}
		availWidth := r.termWidth - indent*2 - 2
		for _, ol := range output {
			display := ol
			if r.termWidth > 0 {
				display = truncateVisible(display, availWidth)
			}
			buf.WriteString(fmt.Sprintf("%s  \033[2m%s\033[0m\n", prefix, display))
			lines++
		}
	}

	for _, child := range item.Children {
		cl, _ := r.renderItem(buf, child, spinner, indent+1)
		lines += cl
	}
	return lines, 0
}

// renderItemFinal renders a single item in final form (no spinner).
func (r *Renderer) renderItemFinal(buf *strings.Builder, item *Item, indent int) {
	prefix := strings.Repeat("  ", indent)
	icon := statusIcon(item.Status)
	line := fmt.Sprintf("%s%s %s", prefix, icon, item.Name)
	if item.Suffix != "" {
		line += " \033[2m" + item.Suffix + "\033[0m"
	}
	buf.WriteString(line + "\n")

	// Show output for failed items (or all if verbose).
	showOutput := item.Status == StatusFailed || r.verbose
	if showOutput && len(item.Output) > 0 {
		for _, ol := range item.Output {
			buf.WriteString(fmt.Sprintf("%s  \033[2m%s\033[0m\n", prefix, ol))
		}
	}

	for _, child := range item.Children {
		r.renderItemFinal(buf, child, indent+1)
	}
}

// visualLines returns how many terminal rows a line occupies, accounting for
// wrapping at the terminal width. Returns 1 when the width is unknown.
func (r *Renderer) visualLines(s string) int {
	if r.termWidth <= 0 {
		return 1
	}
	w := visibleLen(s)
	if w <= r.termWidth {
		return 1
	}
	return (w + r.termWidth - 1) / r.termWidth
}

func (r *Renderer) itemIcon(item *Item, spinner string) string {
	switch item.Status {
	case StatusRunning:
		return "\033[33m" + spinner + "\033[0m"
	default:
		return statusIcon(item.Status)
	}
}

func statusIcon(s Status) string {
	switch s {
	case StatusPassed:
		return "\033[32m✓\033[0m"
	case StatusFailed:
		return "\033[31m✗\033[0m"
	case StatusSkipped:
		return "\033[2m-\033[0m"
	case StatusRunning:
		return "\033[33m⠋\033[0m"
	default:
		return "\033[2m○\033[0m"
	}
}

// PlainIcon returns an uncolored status icon for use in non-TTY summaries.
func PlainIcon(s Status) string {
	switch s {
	case StatusPassed:
		return "✓"
	case StatusFailed:
		return "✗"
	case StatusSkipped:
		return "-"
	default:
		return "○"
	}
}
