package taskui

import (
	"bytes"
	"strings"

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
