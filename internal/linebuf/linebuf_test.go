package linebuf_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/linebuf"
)

var _ = Describe("Buffer", func() {
	It("returns nil for an empty chunk", func() {
		var b linebuf.Buffer
		Expect(b.Append("")).To(BeNil())
		Expect(b.Flush()).To(Equal(""))
	})

	It("emits a single line that ends in \\n", func() {
		var b linebuf.Buffer
		Expect(b.Append("hello\n")).To(Equal([]string{"hello"}))
		Expect(b.Flush()).To(Equal(""))
	})

	It("holds a partial line without a trailing newline", func() {
		var b linebuf.Buffer
		Expect(b.Append("partial")).To(BeNil())
		Expect(b.Flush()).To(Equal("partial"))
	})

	It("splits a multi-line chunk into separate lines", func() {
		var b linebuf.Buffer
		Expect(b.Append("a\nb\nc\n")).To(Equal([]string{"a", "b", "c"}))
		Expect(b.Flush()).To(Equal(""))
	})

	It("joins a partial tail with the following chunk", func() {
		var b linebuf.Buffer
		Expect(b.Append("hel")).To(BeNil())
		Expect(b.Append("lo\nworld")).To(Equal([]string{"hello"}))
		Expect(b.Flush()).To(Equal("world"))
	})

	It("preserves a \\r\\n line ending split across calls", func() {
		var b linebuf.Buffer
		Expect(b.Append("abc\r")).To(BeNil())
		Expect(b.Append("\nfoo\n")).To(Equal([]string{"abc", "foo"}))
		Expect(b.Flush()).To(Equal(""))
	})

	It("strips a trailing \\r from each emitted line", func() {
		var b linebuf.Buffer
		Expect(b.Append("one\r\ntwo\r\n")).To(Equal([]string{"one", "two"}))
	})

	It("preserves blank lines between content", func() {
		var b linebuf.Buffer
		Expect(b.Append("a\n\nb\n")).To(Equal([]string{"a", "", "b"}))
	})

	It("repeated empty Append is a no-op", func() {
		var b linebuf.Buffer
		Expect(b.Append("")).To(BeNil())
		Expect(b.Append("")).To(BeNil())
		Expect(b.Append("ok\n")).To(Equal([]string{"ok"}))
	})

	It("Flush clears the held tail", func() {
		var b linebuf.Buffer
		b.Append("tail")
		Expect(b.Flush()).To(Equal("tail"))
		Expect(b.Flush()).To(Equal(""))
	})
})
