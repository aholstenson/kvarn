package coding

import (
	"strings"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("clampToolText", func() {
	It("leaves a result within the limit untouched", func() {
		Expect(clampToolText("short output\n", 1024, "narrow it")).To(Equal("short output\n"))
	})

	It("leaves a result untouched when there is no limit", func() {
		text := strings.Repeat("x", 100000)
		Expect(clampToolText(text, 0, "")).To(Equal(text))
	})

	It("keeps both ends and drops the middle", func() {
		text := "first line\n" + strings.Repeat("filler line\n", 10000) + "last line\n"

		out := clampToolText(text, 1024, "")
		Expect(out).To(HavePrefix("first line\n"))
		Expect(out).To(HaveSuffix("last line\n"))
		Expect(len(out)).To(BeNumerically("<", 1200))
	})

	It("names how much it dropped", func() {
		// 100K in, 1K retained, so the marker accounts for the other 99K.
		out := clampToolText(strings.Repeat("x", 100*1024), 1024, "")
		Expect(out).To(ContainSubstring("of this result omitted"))
		Expect(out).To(ContainSubstring("99.0K"))
	})

	It("includes the tool's hint so the model can ask a narrower question", func() {
		out := clampToolText(strings.Repeat("x", 100*1024), 1024, "search a narrower path or glob")
		Expect(out).To(ContainSubstring("search a narrower path or glob"))
	})

	It("cuts on line boundaries when one is near the budget", func() {
		text := strings.Repeat("aaaa\n", 1000)

		out := clampToolText(text, 100, "")
		head, rest, found := strings.Cut(out, "\n…[kvarn:")
		Expect(found).To(BeTrue())
		Expect(head).To(HaveSuffix("aaaa\n"))

		_, tail, found := strings.Cut(rest, "]…\n")
		Expect(found).To(BeTrue())
		Expect(tail).To(HavePrefix("aaaa\n"))
	})

	It("still cuts close to the budget when the output has no line breaks", func() {
		out := clampToolText(strings.Repeat("x", 500000), 2048, "")
		Expect(len(out)).To(BeNumerically("<", 2200))
	})

	It("never cuts inside a UTF-8 rune", func() {
		// Three-byte runes guarantee the byte budget lands mid-rune.
		text := strings.Repeat("あ", 20000)

		out := clampToolText(text, 1001, "")
		Expect(utf8.ValidString(out)).To(BeTrue())
	})
})

var _ = Describe("limitForTool", func() {
	It("bounds a tool that has no entry of its own", func() {
		limit := limitForTool("some_tool_added_later")
		Expect(limit.bytes).To(Equal(maxToolResultBytes))
	})

	It("gives file-shaped results more room than command output", func() {
		Expect(limitForTool("read_file").bytes).To(BeNumerically(">", limitForTool("exec_command").bytes))
	})
})
