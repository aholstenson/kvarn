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
// appending "…" when truncated. It assumes s contains no ANSI sequences; use
// truncateVisibleANSI for styled strings.
func truncateVisible(s string, max int) string {
	if max <= 0 {
		return ""
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

// truncateVisibleANSI truncates s to at most max visible runes while preserving
// the ANSI escape sequences it contains, appending an ellipsis and a reset when
// it cuts. Keeping styled lines to a known visible width is what lets the live
// renderer count terminal rows exactly: every emitted line occupies one row,
// so the cursor math in clearActive never drifts.
func truncateVisibleANSI(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if visibleLen(s) <= max {
		return s
	}
	var b strings.Builder
	visible := 0
	for i := 0; i < len(s); {
		if loc := ansiRE.FindStringIndex(s[i:]); loc != nil && loc[0] == 0 {
			b.WriteString(s[i : i+loc[1]])
			i += loc[1]
			continue
		}
		if visible >= max-1 {
			break
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		b.WriteString(s[i : i+size])
		visible++
		i += size
	}
	b.WriteString("…\033[0m")
	return b.String()
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

// terminal reports whether a status will no longer change, which is the
// precondition for committing an item permanently above the live region.
func (s Status) terminal() bool {
	return s == StatusPassed || s == StatusFailed || s == StatusSkipped
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Item represents a single task in the renderer.
type Item struct {
	Name     string
	Status   Status
	Suffix   string
	Output   []string
	Children []*Item

	// Note renders the item as dim free-form text without an icon. It is used
	// for agent narration that sits between tool-call items.
	Note bool

	started  time.Time
	finished time.Time
}

// entry is either a section header or an item pointer.
type entry struct {
	section string // non-empty for section headers
	item    *Item
}

// Renderer draws a live task list to a terminal.
//
// Finished work is committed: once the leading entries are all in a terminal
// state they are printed once, permanently, and drop out of the redraw set.
// Only the trailing "live" region (the in-flight items and their output) is
// repainted each frame, and it is clamped to the terminal height. ANSI cursor
// movement cannot address rows that have scrolled off-screen, so bounding the
// live region this way is what prevents the redraw from duplicating earlier
// items once a run grows past one screenful.
type Renderer struct {
	mu          sync.Mutex
	w           io.Writer
	fd          int // terminal file descriptor, or -1 when w is not a TTY file
	isTTY       bool
	verbose     bool
	liveLines   int
	bufferLines int
	termWidth   int
	termHeight  int
	entries     []entry
	flushedIdx  int // entries[:flushedIdx] are committed and never redrawn
	drawnLines  int // rows the live region currently occupies on screen
	stopCh      chan struct{}
	stopOnce    sync.Once
	spinIdx     int
}

// New creates a renderer. It detects whether w is a TTY (when w is os.Stdout
// or os.Stderr). When verbose is true, all output lines are kept; otherwise
// the buffer is capped so failed items can replay the last bufferLines lines
// at the end, while the live redraw region is bounded to liveLines.
func New(w io.Writer, verbose bool) *Renderer {
	isTTY := false
	fd := -1
	termWidth, termHeight := 0, 0
	if f, ok := w.(*os.File); ok {
		fd = int(f.Fd())
		isTTY = term.IsTerminal(fd)
		if isTTY {
			if cw, ch, err := term.GetSize(fd); err == nil {
				termWidth, termHeight = cw, ch
			}
		}
	}
	return &Renderer{
		w:           w,
		fd:          fd,
		isTTY:       isTTY,
		verbose:     verbose,
		liveLines:   20,
		bufferLines: 200,
		termWidth:   termWidth,
		termHeight:  termHeight,
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
		r.mu.Lock()
		defer r.mu.Unlock()
		// Clear the live region and do a final render. Closing stopCh inside
		// the lock ensures the spinner cannot redraw after the final render.
		r.clearActive()
		r.renderFinal()
		// Show cursor and stop the spinner goroutine.
		fmt.Fprint(r.w, "\033[?25h")
		close(r.stopCh)
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
			// Re-check whether Stop() has finished while we waited for the lock.
			select {
			case <-r.stopCh:
				r.mu.Unlock()
				return
			default:
			}
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

// AddNote adds a top-level note: dim free-form text (e.g. agent narration)
// that commits to the scrollback immediately, in stream order with the items
// around it.
func (r *Renderer) AddNote(text string) *Item {
	r.mu.Lock()
	defer r.mu.Unlock()
	item := &Item{Name: text, Note: true, Status: StatusPassed}
	r.entries = append(r.entries, entry{item: item})
	if !r.isTTY {
		for _, ln := range strings.Split(text, "\n") {
			fmt.Fprintln(r.w, ln)
		}
	}
	return item
}

// AddChild adds a child item under a parent.
func (r *Renderer) AddChild(parent *Item, name string) *Item {
	r.mu.Lock()
	defer r.mu.Unlock()
	child := &Item{Name: name, Status: StatusPending}
	parent.Children = append(parent.Children, child)
	return child
}

// Items returns the top-level items in order (excluding section headers). It is
// primarily useful for inspecting renderer state in tests.
func (r *Renderer) Items() []*Item {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]*Item, 0, len(r.entries))
	for _, e := range r.entries {
		if e.item != nil {
			items = append(items, e.item)
		}
	}
	return items
}

// SetStatus updates an item's status and suffix.
func (r *Renderer) SetStatus(item *Item, status Status, suffix string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applyStatus(item, status, suffix)
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

// applyStatus mutates the item and stamps run timings so the renderer can show
// elapsed time on running items and total duration on finished ones. Caller
// must hold r.mu.
func (r *Renderer) applyStatus(item *Item, status Status, suffix string) {
	if status == StatusRunning && item.started.IsZero() {
		item.started = time.Now()
	}
	if status.terminal() && item.finished.IsZero() {
		item.finished = time.Now()
	}
	item.Status = status
	item.Suffix = suffix
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

// clearActive moves the cursor up and clears the live region's drawn lines.
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

// refreshSize re-reads the terminal dimensions so a mid-run resize is picked up
// on the next frame. Caller must hold r.mu.
func (r *Renderer) refreshSize() {
	if !r.isTTY || r.fd < 0 {
		return
	}
	if cw, ch, err := term.GetSize(r.fd); err == nil {
		r.termWidth, r.termHeight = cw, ch
	}
}

// render redraws the active region (TTY only). Caller must hold r.mu.
func (r *Renderer) render() {
	r.refreshSize()
	r.clearActive()

	// Commit the longest leading run of stable entries: print them once, in
	// final form, where they become permanent scrollback above the live
	// region and are never redrawn again.
	commitTo := r.flushedIdx
	for commitTo < len(r.entries) && r.entryStable(r.entries[commitTo]) {
		commitTo++
	}
	if commitTo > r.flushedIdx {
		var cb strings.Builder
		for _, e := range r.entries[r.flushedIdx:commitTo] {
			r.writeEntryFinal(&cb, e)
		}
		fmt.Fprint(r.w, cb.String())
		r.flushedIdx = commitTo
	}

	// Build and draw the live region. Every row is fit to one terminal line so
	// the row count is exact, then the whole region is clamped to the viewport.
	rows := r.clampRows(r.liveRows())
	var buf strings.Builder
	for _, row := range rows {
		buf.WriteString(row)
		buf.WriteByte('\n')
	}
	fmt.Fprint(r.w, buf.String())
	r.drawnLines = len(rows)
}

// renderFinal writes the still-live entries in final form without spinners,
// then marks everything committed. Caller must hold r.mu.
func (r *Renderer) renderFinal() {
	var buf strings.Builder
	for _, e := range r.entries[r.flushedIdx:] {
		r.writeEntryFinal(&buf, e)
	}
	fmt.Fprint(r.w, buf.String())
	r.flushedIdx = len(r.entries)
}

// entryStable reports whether an entry will no longer change and can therefore
// be committed. Section headers are always stable; an item is stable once it
// and all of its descendants have reached a terminal status.
func (r *Renderer) entryStable(e entry) bool {
	if e.section != "" {
		return true
	}
	return itemTerminal(e.item)
}

func itemTerminal(item *Item) bool {
	if item.Note {
		return true
	}
	if !item.Status.terminal() {
		return false
	}
	for _, c := range item.Children {
		if !itemTerminal(c) {
			return false
		}
	}
	return true
}

// liveRows renders the uncommitted entries into one-row-per-string lines.
func (r *Renderer) liveRows() []string {
	spinner := spinnerFrames[r.spinIdx]
	var rows []string
	for _, e := range r.entries[r.flushedIdx:] {
		if e.section != "" {
			rows = append(rows, r.fitRow(fmt.Sprintf("\033[1m=== %s ===\033[0m", e.section)))
			continue
		}
		rows = append(rows, r.itemRows(e.item, spinner, 0)...)
	}
	return rows
}

// itemRows renders a single item (and its children) as one-row-per-string
// lines for the live region.
func (r *Renderer) itemRows(item *Item, spinner string, indent int) []string {
	prefix := strings.Repeat("  ", indent)
	if item.Note {
		var rows []string
		for _, ln := range strings.Split(item.Name, "\n") {
			rows = append(rows, r.fitRow(prefix+"\033[2m"+ln+"\033[0m"))
		}
		return rows
	}

	icon := r.itemIcon(item, spinner)
	header := fmt.Sprintf("%s%s %s", prefix, icon, item.Name)
	if item.Suffix != "" {
		header += " \033[2m" + item.Suffix + "\033[0m"
	}
	if d := durationSuffix(item); d != "" {
		header += " \033[2m" + d + "\033[0m"
	}
	rows := []string{r.fitRow(header)}

	// In the live render only show output for running or failed items, capped
	// to liveLines to keep the redraw zone bounded. The full buffered output is
	// shown by renderFinal / commit when the item reaches a terminal state.
	if (item.Status == StatusRunning || item.Status == StatusFailed) && len(item.Output) > 0 {
		output := item.Output
		if len(output) > r.liveLines {
			output = output[len(output)-r.liveLines:]
		}
		for _, ol := range output {
			rows = append(rows, r.fitRow(fmt.Sprintf("%s  \033[2m%s\033[0m", prefix, ol)))
		}
	}

	for _, child := range item.Children {
		rows = append(rows, r.itemRows(child, spinner, indent+1)...)
	}
	return rows
}

// clampRows bounds the live region to the viewport height. When it overflows,
// the active header (first row) is kept and the most recent rows are shown,
// since ANSI cursor movement cannot reach rows that have scrolled away.
func (r *Renderer) clampRows(rows []string) []string {
	if r.termHeight <= 0 {
		return rows
	}
	max := r.termHeight - 1
	if max < 1 {
		max = 1
	}
	if len(rows) <= max {
		return rows
	}
	if max == 1 {
		return rows[len(rows)-1:]
	}
	out := make([]string, 0, max)
	out = append(out, rows[0])
	out = append(out, rows[len(rows)-(max-1):]...)
	return out
}

// fitRow truncates a styled row to the terminal width so it occupies exactly
// one terminal line. With an unknown width it is returned unchanged.
func (r *Renderer) fitRow(s string) string {
	if r.termWidth <= 0 {
		return s
	}
	return truncateVisibleANSI(s, r.termWidth)
}

// writeEntryFinal writes a committed entry in its final form (no spinner). For
// items this includes failed output (or all output in verbose mode); committed
// lines may wrap freely since they are never repositioned.
func (r *Renderer) writeEntryFinal(buf *strings.Builder, e entry) {
	if e.section != "" {
		buf.WriteString(fmt.Sprintf("\n\033[1m=== %s ===\033[0m\n", e.section))
		return
	}
	r.writeItemFinal(buf, e.item, 0)
}

func (r *Renderer) writeItemFinal(buf *strings.Builder, item *Item, indent int) {
	prefix := strings.Repeat("  ", indent)
	if item.Note {
		for _, ln := range strings.Split(item.Name, "\n") {
			buf.WriteString(prefix + "\033[2m" + ln + "\033[0m\n")
		}
		return
	}

	icon := statusIcon(item.Status)
	line := fmt.Sprintf("%s%s %s", prefix, icon, item.Name)
	if item.Suffix != "" {
		line += " \033[2m" + item.Suffix + "\033[0m"
	}
	if d := durationSuffix(item); d != "" {
		line += " \033[2m" + d + "\033[0m"
	}
	buf.WriteString(line + "\n")

	// Show output for failed items (or all if verbose).
	if (item.Status == StatusFailed || r.verbose) && len(item.Output) > 0 {
		for _, ol := range item.Output {
			buf.WriteString(fmt.Sprintf("%s  \033[2m%s\033[0m\n", prefix, ol))
		}
	}

	for _, child := range item.Children {
		r.writeItemFinal(buf, child, indent+1)
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

// durationSuffix formats the elapsed time for a running item or the total
// duration for a finished one. It returns "" for sub-second or untimed items so
// instant steps stay uncluttered.
func durationSuffix(item *Item) string {
	var d time.Duration
	switch {
	case item.Status == StatusRunning && !item.started.IsZero():
		d = time.Since(item.started)
	case !item.started.IsZero() && !item.finished.IsZero():
		d = item.finished.Sub(item.started)
	default:
		return ""
	}
	return formatDuration(d)
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return ""
	}
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int((d%time.Minute)/time.Second))
	default:
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int((d%time.Hour)/time.Minute))
	}
}
