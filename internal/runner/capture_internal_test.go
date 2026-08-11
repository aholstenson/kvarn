package runner

import (
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("capBuffer", func() {
	It("keeps everything when nothing exceeds the limit", func() {
		b := newCapBuffer(100)
		_, err := b.Write([]byte("hello world"))
		Expect(err).NotTo(HaveOccurred())

		Expect(b.String()).To(Equal("hello world"))
		Expect(b.Truncated()).To(BeFalse())
		Expect(b.Total()).To(Equal(int64(11)))
	})

	It("keeps everything when the limit is zero", func() {
		b := newCapBuffer(0)
		_, err := b.Write([]byte(strings.Repeat("x", 10000)))
		Expect(err).NotTo(HaveOccurred())

		Expect(b.String()).To(HaveLen(10000))
		Expect(b.Truncated()).To(BeFalse())
	})

	It("keeps both ends and drops the middle", func() {
		b := newCapBuffer(20)
		_, err := b.Write([]byte("HEAD" + strings.Repeat("x", 1000) + "TAIL"))
		Expect(err).NotTo(HaveOccurred())

		out := b.String()
		Expect(out).To(HavePrefix("HEAD"))
		Expect(out).To(HaveSuffix("TAIL"))
		Expect(out).To(ContainSubstring("of output omitted"))
		Expect(b.Truncated()).To(BeTrue())
		Expect(b.Total()).To(Equal(int64(1008)))
	})

	It("retains the two ends across many small writes", func() {
		b := newCapBuffer(20)
		for _, chunk := range []string{"AAAA", "bbbb", "cccc", "dddd", "eeee", "ZZZZ"} {
			_, err := b.Write([]byte(chunk))
			Expect(err).NotTo(HaveOccurred())
		}

		out := b.String()
		Expect(out).To(HavePrefix("AAAA"))
		Expect(out).To(HaveSuffix("ZZZZ"))
		Expect(b.Total()).To(Equal(int64(24)))
		Expect(b.Truncated()).To(BeTrue())
	})

	It("reports the dropped byte count in the marker", func() {
		b := newCapBuffer(16)
		_, err := b.Write([]byte(strings.Repeat("x", 16+2048)))
		Expect(err).NotTo(HaveOccurred())

		// 2048 bytes beyond the retained 16 leaves exactly 2K dropped.
		Expect(b.String()).To(ContainSubstring("2.0K of output omitted"))
	})

	It("holds no more than the limit regardless of how much is written", func() {
		b := newCapBuffer(64)
		for range 100 {
			_, err := b.Write([]byte(strings.Repeat("y", 1024)))
			Expect(err).NotTo(HaveOccurred())
		}

		Expect(len(b.head) + len(b.tail)).To(Equal(64))
		Expect(b.Total()).To(Equal(int64(102400)))
	})
})

var _ = Describe("readCappedFile", func() {
	var dir string

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
	})

	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		Expect(os.WriteFile(path, []byte(content), 0o644)).To(Succeed())
		return path
	}

	It("returns a small file whole", func() {
		path := write("small", "hello\n")

		text, total, truncated := readCappedFile(path, 1024)
		Expect(text).To(Equal("hello\n"))
		Expect(total).To(Equal(int64(6)))
		Expect(truncated).To(BeFalse())
	})

	It("returns the whole file when uncapped", func() {
		path := write("big", strings.Repeat("z", 5000))

		text, total, truncated := readCappedFile(path, 0)
		Expect(text).To(HaveLen(5000))
		Expect(total).To(Equal(int64(5000)))
		Expect(truncated).To(BeFalse())
	})

	It("reads only the two ends of a large file", func() {
		path := write("huge", "HEAD"+strings.Repeat("m", 100000)+"TAIL")

		text, total, truncated := readCappedFile(path, 100)
		Expect(text).To(HavePrefix("HEAD"))
		Expect(text).To(HaveSuffix("TAIL"))
		Expect(text).To(ContainSubstring("of output omitted"))
		Expect(total).To(Equal(int64(100008)))
		Expect(truncated).To(BeTrue())
		// Retained content plus the marker, nowhere near the file's size.
		Expect(len(text)).To(BeNumerically("<", 200))
	})

	It("reads a missing file as empty", func() {
		text, total, truncated := readCappedFile(filepath.Join(dir, "absent"), 1024)
		Expect(text).To(BeEmpty())
		Expect(total).To(BeZero())
		Expect(truncated).To(BeFalse())
	})
})
