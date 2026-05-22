package taskui

import (
	"bytes"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func newPlainRenderer(buf *bytes.Buffer, opts ...func(*Renderer)) *Renderer {
	r := &Renderer{
		w:           buf,
		isTTY:       false,
		liveLines:   10,
		bufferLines: 10,
		stopCh:      make(chan struct{}),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// newTTYRenderer builds a renderer that renders into buf as if it were a TTY of
// the given size. fd is -1 so refreshSize is a no-op and the fixed dimensions
// are honored, letting the redraw path be exercised without a real terminal.
func newTTYRenderer(buf *bytes.Buffer, width, height int) *Renderer {
	return &Renderer{
		w:           buf,
		fd:          -1,
		isTTY:       true,
		liveLines:   20,
		bufferLines: 200,
		termWidth:   width,
		termHeight:  height,
		stopCh:      make(chan struct{}),
	}
}

var _ = Describe("Renderer (plain mode)", func() {
	var (
		buf *bytes.Buffer
		r   *Renderer
	)

	BeforeEach(func() {
		buf = &bytes.Buffer{}
		r = newPlainRenderer(buf)
	})

	Describe("status icons", func() {
		It("shows ✓ for passed items", func() {
			item := r.AddItem("lint")
			r.SetStatus(item, StatusPassed, "")
			Expect(buf.String()).To(ContainSubstring("✓ lint"))
		})

		It("shows ✗ for failed items", func() {
			item := r.AddItem("test")
			r.SetStatus(item, StatusFailed, "")
			Expect(buf.String()).To(ContainSubstring("✗ test"))
		})

		It("shows - for skipped items", func() {
			item := r.AddItem("format")
			r.SetStatus(item, StatusSkipped, "")
			Expect(buf.String()).To(ContainSubstring("- format"))
		})
	})

	Describe("suffixes", func() {
		It("appends the suffix after the item name", func() {
			item := r.AddItem("format")
			r.SetStatus(item, StatusSkipped, "(no matching files)")
			Expect(buf.String()).To(ContainSubstring("- format (no matching files)"))
		})
	})

	Describe("section headers", func() {
		It("prints section headers", func() {
			r.AddSection("Setup")
			Expect(buf.String()).To(ContainSubstring("=== Setup ==="))
		})
	})

	Describe("output buffering", func() {
		It("limits output to bufferLines lines", func() {
			r = newPlainRenderer(buf, func(r *Renderer) { r.bufferLines = 3 })
			item := r.AddItem("build")
			for i := range 10 {
				r.AppendOutput(item, strings.Repeat("x", i+1))
			}
			Expect(item.Output).To(HaveLen(3))
			Expect(item.Output[0]).To(Equal("xxxxxxxx"))
		})

		It("keeps all output in verbose mode", func() {
			r = newPlainRenderer(buf, func(r *Renderer) { r.verbose = true })
			item := r.AddItem("build")
			for range 10 {
				r.AppendOutput(item, "line")
			}
			Expect(item.Output).To(HaveLen(10))
		})
	})

	Describe("ANSI stripping", func() {
		It("strips CSI escape sequences from output", func() {
			item := r.AddItem("build")
			r.AppendOutput(item, "\033[31mred text\033[0m")
			Expect(item.Output[0]).To(Equal("red text"))
		})

		It("strips cursor movement sequences", func() {
			item := r.AddItem("build")
			r.AppendOutput(item, "\033[2Asome text\033[K")
			Expect(item.Output[0]).To(Equal("some text"))
		})

		It("handles carriage returns by keeping text after last CR", func() {
			item := r.AddItem("build")
			r.AppendOutput(item, "old progress\rnew progress")
			Expect(item.Output[0]).To(Equal("new progress"))
		})

		It("strips OSC sequences", func() {
			item := r.AddItem("build")
			r.AppendOutput(item, "\033]0;window title\x07actual output")
			Expect(item.Output[0]).To(Equal("actual output"))
		})
	})

	Describe("truncation", func() {
		It("truncates lines longer than max width", func() {
			Expect(truncateVisible("hello world", 5)).To(Equal("hell…"))
		})

		It("does not truncate lines within max width", func() {
			Expect(truncateVisible("hello", 10)).To(Equal("hello"))
		})

		It("handles exact width", func() {
			Expect(truncateVisible("hello", 5)).To(Equal("hello"))
		})
	})

	Describe("visual line counting", func() {
		It("returns 1 when termWidth is 0", func() {
			r.termWidth = 0
			Expect(r.visualLines("a long line that would wrap")).To(Equal(1))
		})

		It("returns 1 for short lines", func() {
			r.termWidth = 80
			Expect(r.visualLines("short")).To(Equal(1))
		})

		It("counts wrapped lines", func() {
			r.termWidth = 10
			Expect(r.visualLines(strings.Repeat("x", 25))).To(Equal(3))
		})

		It("ignores ANSI sequences when measuring", func() {
			r.termWidth = 10
			// 5 visible chars + ANSI codes = should be 1 line
			Expect(r.visualLines("\033[31mhello\033[0m")).To(Equal(1))
		})
	})

	Describe("failed output", func() {
		It("prints buffered output when an item fails", func() {
			item := r.AddItem("test")
			r.AppendOutput(item, "error: something broke")
			r.SetStatus(item, StatusFailed, "")
			Expect(buf.String()).To(ContainSubstring("error: something broke"))
		})

		It("does not print output for passing items", func() {
			item := r.AddItem("test")
			r.AppendOutput(item, "all good")
			r.SetStatus(item, StatusPassed, "")
			Expect(buf.String()).NotTo(ContainSubstring("all good"))
		})
	})
})

var _ = Describe("AddChild", func() {
	It("initializes child status to StatusPending, not StatusRunning", func() {
		buf := &bytes.Buffer{}
		r := newPlainRenderer(buf)
		parent := r.AddItem("parent")
		child := r.AddChild(parent, "child")
		Expect(child.Status).To(Equal(StatusPending))
	})
})

var _ = Describe("truncateVisible (edge cases)", func() {
	It("returns empty string when max is zero", func() {
		Expect(truncateVisible("hello", 0)).To(Equal(""))
	})

	It("returns empty string when max is negative", func() {
		Expect(truncateVisible("hello", -5)).To(Equal(""))
	})
})

var _ = Describe("truncateVisibleANSI", func() {
	It("leaves a styled string within width untouched", func() {
		s := "\033[31mhello\033[0m"
		Expect(truncateVisibleANSI(s, 10)).To(Equal(s))
	})

	It("truncates by visible width while preserving ANSI codes", func() {
		out := truncateVisibleANSI("\033[31mhello world\033[0m", 5)
		Expect(visibleLen(out)).To(Equal(5)) // 4 visible runes + ellipsis
		Expect(out).To(ContainSubstring("\033[31m"))
		Expect(out).To(HaveSuffix("…\033[0m"))
		Expect(out).To(ContainSubstring("hell"))
	})

	It("does not count ANSI codes toward the width budget", func() {
		// 5 visible chars wrapped in codes: fits in width 5, so unchanged.
		s := "\033[1m\033[33mhi th\033[0m"
		Expect(truncateVisibleANSI(s, 5)).To(Equal(s))
	})
})

var _ = Describe("formatDuration", func() {
	It("returns empty for sub-second durations", func() {
		Expect(formatDuration(500 * time.Millisecond)).To(Equal(""))
	})

	It("formats seconds", func() {
		Expect(formatDuration(8 * time.Second)).To(Equal("8s"))
	})

	It("formats minutes and seconds", func() {
		Expect(formatDuration(72 * time.Second)).To(Equal("1m12s"))
	})

	It("formats hours and minutes", func() {
		Expect(formatDuration(90 * time.Minute)).To(Equal("1h30m"))
	})
})

var _ = Describe("itemTerminal", func() {
	It("is false for pending and running items", func() {
		Expect(itemTerminal(&Item{Status: StatusPending})).To(BeFalse())
		Expect(itemTerminal(&Item{Status: StatusRunning})).To(BeFalse())
	})

	It("is true for terminal statuses", func() {
		Expect(itemTerminal(&Item{Status: StatusPassed})).To(BeTrue())
		Expect(itemTerminal(&Item{Status: StatusFailed})).To(BeTrue())
		Expect(itemTerminal(&Item{Status: StatusSkipped})).To(BeTrue())
	})

	It("treats notes as terminal", func() {
		Expect(itemTerminal(&Item{Note: true})).To(BeTrue())
	})

	It("is false when any child is not terminal", func() {
		parent := &Item{Status: StatusPassed, Children: []*Item{
			{Status: StatusRunning},
		}}
		Expect(itemTerminal(parent)).To(BeFalse())
	})
})

var _ = Describe("clampRows", func() {
	var r *Renderer

	BeforeEach(func() {
		r = newPlainRenderer(&bytes.Buffer{})
	})

	It("returns rows unchanged when height is unknown", func() {
		r.termHeight = 0
		rows := []string{"a", "b", "c"}
		Expect(r.clampRows(rows)).To(Equal(rows))
	})

	It("returns rows unchanged when they fit", func() {
		r.termHeight = 10
		rows := []string{"a", "b", "c"}
		Expect(r.clampRows(rows)).To(Equal(rows))
	})

	It("keeps the header row and the most recent rows when overflowing", func() {
		r.termHeight = 4 // max live rows = 3
		rows := []string{"header", "1", "2", "3", "4", "5"}
		Expect(r.clampRows(rows)).To(Equal([]string{"header", "4", "5"}))
	})
})

var _ = Describe("Items", func() {
	It("returns top-level items excluding section headers", func() {
		r := newPlainRenderer(&bytes.Buffer{})
		r.AddSection("Setup")
		a := r.AddItem("a")
		r.AddSection("Tests")
		b := r.AddItem("b")
		Expect(r.Items()).To(Equal([]*Item{a, b}))
	})
})

var _ = Describe("AddNote", func() {
	It("prints note text in plain mode", func() {
		buf := &bytes.Buffer{}
		r := newPlainRenderer(buf)
		r.AddNote("line one\nline two")
		Expect(buf.String()).To(ContainSubstring("line one"))
		Expect(buf.String()).To(ContainSubstring("line two"))
	})
})

var _ = Describe("committed-region rendering", func() {
	It("commits the leading stable prefix and keeps only live items in the redraw region", func() {
		buf := &bytes.Buffer{}
		r := newTTYRenderer(buf, 80, 24)

		done := r.AddItem("setup")
		r.applyStatus(done, StatusPassed, "")
		running := r.AddItem("agent")
		r.applyStatus(running, StatusRunning, "")

		r.mu.Lock()
		r.render()
		r.mu.Unlock()

		// "setup" is committed (printed permanently); only "agent" stays live.
		Expect(r.flushedIdx).To(Equal(1))
		Expect(r.drawnLines).To(Equal(1))
		Expect(buf.String()).To(ContainSubstring("setup"))
		Expect(buf.String()).To(ContainSubstring("agent"))

		// Finishing the live item commits it on the next frame.
		r.applyStatus(running, StatusPassed, "")
		r.mu.Lock()
		r.render()
		r.mu.Unlock()
		Expect(r.flushedIdx).To(Equal(2))
		Expect(r.drawnLines).To(Equal(0))
	})

	It("never lets the redraw region exceed the viewport height", func() {
		buf := &bytes.Buffer{}
		r := newTTYRenderer(buf, 80, 6) // max live rows = 5

		item := r.AddItem("agent")
		r.applyStatus(item, StatusRunning, "")
		for i := range 50 {
			r.AppendOutput(item, strings.Repeat("x", i+1))
		}

		r.mu.Lock()
		r.render()
		r.mu.Unlock()

		Expect(r.drawnLines).To(BeNumerically("<=", 5))
		Expect(r.drawnLines).To(BeNumerically(">", 0))
	})
})

var _ = Describe("elapsed/duration suffix", func() {
	It("reports elapsed time for a running item", func() {
		item := &Item{Status: StatusRunning, started: time.Now().Add(-5 * time.Second)}
		Expect(durationSuffix(item)).To(Equal("5s"))
	})

	It("reports total duration for a finished item", func() {
		start := time.Now().Add(-90 * time.Second)
		item := &Item{Status: StatusPassed, started: start, finished: start.Add(72 * time.Second)}
		Expect(durationSuffix(item)).To(Equal("1m12s"))
	})

	It("returns empty for an untimed item", func() {
		Expect(durationSuffix(&Item{Status: StatusPending})).To(Equal(""))
	})
})
