package attachment_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/attachment"
)

// pngBytes encodes a small real image, so the header check has something it
// can read.
func pngBytes() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 2, 3))
	img.Set(0, 0, color.White)
	var buf bytes.Buffer
	Expect(png.Encode(&buf, img)).To(Succeed())
	return buf.Bytes()
}

var _ = Describe("Prepare", func() {
	It("accepts no attachments", func() {
		got, err := attachment.Prepare(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeNil())
	})

	It("detects the media type from the content when none is declared", func() {
		got, err := attachment.Prepare([]attachment.File{
			{Name: "shot.png", Data: pngBytes()},
			{Name: "notes.md", Data: []byte("# Notes\n")},
			{Name: "doc.pdf", Data: []byte("%PDF-1.7\n...")},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(got[0].MediaType).To(Equal("image/png"))
		Expect(got[1].MediaType).To(Equal("text/plain; charset=utf-8"))
		Expect(got[2].MediaType).To(Equal("application/pdf"))
	})

	It("keeps a declared type, in canonical form", func() {
		got, err := attachment.Prepare([]attachment.File{
			{Name: "data.json", MediaType: "Application/JSON", Data: []byte(`{"a":1}`)},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(got[0].MediaType).To(Equal("application/json"))
	})

	It("does not change the files it was given", func() {
		in := []attachment.File{{Name: "notes.md", Data: []byte("x")}}
		_, err := attachment.Prepare(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(in[0].MediaType).To(BeEmpty())
	})

	It("refuses an image type the content does not match", func() {
		_, err := attachment.Prepare([]attachment.File{
			{Name: "shot.png", MediaType: "image/png", Data: []byte("not an image")},
		})
		Expect(err).To(MatchError(ContainSubstring("declared as image/png, but the content is text/plain")))
	})

	It("refuses content that is an image declared as something else", func() {
		_, err := attachment.Prepare([]attachment.File{
			{Name: "shot.txt", MediaType: "text/plain", Data: pngBytes()},
		})
		Expect(err).To(MatchError(ContainSubstring("declared as text/plain, but the content is image/png")))
	})

	It("refuses an image whose header cannot be read", func() {
		truncated := pngBytes()[:20]
		_, err := attachment.Prepare([]attachment.File{{Name: "shot.png", Data: truncated}})
		Expect(err).To(MatchError(ContainSubstring("not a readable image/png")))
	})

	It("refuses a media type that does not parse", func() {
		_, err := attachment.Prepare([]attachment.File{{Name: "a", MediaType: "not a type", Data: []byte("x")}})
		Expect(err).To(MatchError(ContainSubstring(`media type "not a type"`)))
	})

	It("refuses the same name twice", func() {
		_, err := attachment.Prepare([]attachment.File{
			{Name: "a.txt", Data: []byte("1")},
			{Name: "a.txt", Data: []byte("2")},
		})
		Expect(err).To(MatchError(ContainSubstring(`"a.txt" is given more than once`)))
	})

	It("refuses more files than the limit", func() {
		files := make([]attachment.File, attachment.MaxFiles+1)
		for i := range files {
			files[i] = attachment.File{Name: strings.Repeat("a", i+1), Data: []byte("x")}
		}
		_, err := attachment.Prepare(files)
		Expect(err).To(MatchError(ContainSubstring("the limit is 20")))
	})

	It("refuses a file over the size limit", func() {
		_, err := attachment.Prepare([]attachment.File{
			{Name: "big.bin", Data: make([]byte, attachment.MaxFileBytes+1)},
		})
		Expect(err).To(MatchError(ContainSubstring(`"big.bin" is 10485761 bytes`)))
	})

	It("refuses files over the total limit together", func() {
		files := make([]attachment.File, 4)
		for i := range files {
			files[i] = attachment.File{Name: strings.Repeat("b", i+1), Data: make([]byte, attachment.MaxFileBytes)}
		}
		_, err := attachment.Prepare(files)
		Expect(err).To(MatchError(ContainSubstring("the limit is 33554432")))
	})

	It("accepts files at exactly the limits", func() {
		files := []attachment.File{
			{Name: "a", Data: make([]byte, attachment.MaxFileBytes)},
			{Name: "b", Data: make([]byte, attachment.MaxFileBytes)},
			{Name: "c", Data: make([]byte, attachment.MaxFileBytes)},
			{Name: "d", Data: make([]byte, attachment.MaxTotalBytes-3*attachment.MaxFileBytes)},
		}
		_, err := attachment.Prepare(files)
		Expect(err).NotTo(HaveOccurred())
	})

	DescribeTable("delivery",
		func(d attachment.Delivery, wantErr string) {
			_, err := attachment.Prepare([]attachment.File{{Name: "a.txt", Delivery: d, Data: []byte("x")}})
			if wantErr == "" {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(MatchError(ContainSubstring(wantErr)))
			}
		},
		Entry("auto", attachment.DeliveryAuto, ""),
		Entry("vm", attachment.DeliveryVM, ""),
		Entry("prompt", attachment.DeliveryPrompt, ""),
		Entry("both", attachment.DeliveryBoth, ""),
		Entry("unknown", attachment.Delivery("disk"), `unknown delivery "disk"`),
	)

	Describe("files asked into the prompt", func() {
		It("accepts images, PDFs and text up to the text limit", func() {
			_, err := attachment.Prepare([]attachment.File{
				{Name: "shot.png", Delivery: attachment.DeliveryPrompt, Data: pngBytes()},
				{Name: "doc.pdf", Delivery: attachment.DeliveryBoth, Data: []byte("%PDF-1.7\n")},
				{Name: "spec.md", Delivery: attachment.DeliveryPrompt, Data: bytes.Repeat([]byte("a"), attachment.MaxPromptTextBytes)},
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("refuses a text file over the text limit", func() {
			big := bytes.Repeat([]byte("a"), attachment.MaxPromptTextBytes+1)
			_, err := attachment.Prepare([]attachment.File{
				{Name: "log.txt", Delivery: attachment.DeliveryBoth, Data: big},
			})
			Expect(err).To(MatchError(ContainSubstring("a text file in the prompt is at most 262144 bytes")))
		})

		It("refuses a type no model reads in a message", func() {
			_, err := attachment.Prepare([]attachment.File{
				{Name: "data.bin", Delivery: attachment.DeliveryPrompt, Data: []byte{0, 1, 2, 0xff}},
			})
			Expect(err).To(MatchError(ContainSubstring("a application/octet-stream file cannot go into the prompt")))
		})

		It("does not hold files bound for the VM to the prompt rules", func() {
			_, err := attachment.Prepare([]attachment.File{
				{Name: "data.bin", Data: []byte{0, 1, 2, 0xff}},
				{Name: "log.txt", Delivery: attachment.DeliveryVM, Data: bytes.Repeat([]byte("a"), attachment.MaxPromptTextBytes+1)},
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

var _ = Describe("File.PromptCandidate", func() {
	DescribeTable("offers the prompt what may go there",
		func(d attachment.Delivery, mediaType string, want bool) {
			f := attachment.File{Name: "f", MediaType: mediaType, Delivery: d}
			Expect(f.PromptCandidate()).To(Equal(want))
		},
		Entry("an automatic image", attachment.DeliveryAuto, "image/png", true),
		Entry("an automatic PDF", attachment.DeliveryAuto, "application/pdf", true),
		Entry("automatic text stays in the VM", attachment.DeliveryAuto, "text/plain; charset=utf-8", false),
		Entry("a VM-only image", attachment.DeliveryVM, "image/png", false),
		Entry("text asked into the prompt", attachment.DeliveryPrompt, "text/plain", true),
		Entry("text asked into both", attachment.DeliveryBoth, "application/json", true),
	)
})

var _ = Describe("PromptKind", func() {
	DescribeTable("classifies media types",
		func(mediaType, want string) { Expect(attachment.PromptKind(mediaType)).To(Equal(want)) },
		Entry("png", "image/png", attachment.KindImage),
		Entry("pdf", "application/pdf", attachment.KindPDF),
		Entry("text with charset", "text/plain; charset=utf-8", attachment.KindText),
		Entry("json", "application/json", attachment.KindText),
		Entry("a +json type", "application/vnd.api+json", attachment.KindText),
		Entry("yaml", "application/yaml", attachment.KindText),
		Entry("binary", "application/octet-stream", ""),
		Entry("unparseable", "not a type", ""),
	)
})

var _ = Describe("ParseArg", func() {
	DescribeTable("reads an optional delivery before the path",
		func(arg string, wantDelivery attachment.Delivery, wantPath string) {
			d, p, err := attachment.ParseArg(arg)
			Expect(err).NotTo(HaveOccurred())
			Expect(d).To(Equal(wantDelivery))
			Expect(p).To(Equal(wantPath))
		},
		Entry("a plain path", "shot.png", attachment.DeliveryAuto, "shot.png"),
		Entry("prompt", "prompt=shot.png", attachment.DeliveryPrompt, "shot.png"),
		Entry("both", "both=dir/spec.md", attachment.DeliveryBoth, "dir/spec.md"),
		Entry("vm", "vm=log.txt", attachment.DeliveryVM, "log.txt"),
		Entry("auto by name", "auto=log.txt", attachment.DeliveryAuto, "log.txt"),
		Entry("a path with = that is not a delivery", "a=b.txt", attachment.DeliveryAuto, "a=b.txt"),
		Entry("a path escaped with ./", "./vm=x", attachment.DeliveryAuto, "./vm=x"),
	)

	It("refuses a delivery with no path", func() {
		_, _, err := attachment.ParseArg("prompt=")
		Expect(err).To(MatchError(ContainSubstring("names no file")))
	})
})

var _ = Describe("Delivery", func() {
	It("puts every delivery except prompt in the VM", func() {
		Expect(attachment.DeliveryAuto.InVM()).To(BeTrue())
		Expect(attachment.DeliveryVM.InVM()).To(BeTrue())
		Expect(attachment.DeliveryBoth.InVM()).To(BeTrue())
		Expect(attachment.DeliveryPrompt.InVM()).To(BeFalse())
	})

	It("insists on the prompt only for prompt and both", func() {
		Expect(attachment.DeliveryAuto.InPrompt()).To(BeFalse())
		Expect(attachment.DeliveryVM.InPrompt()).To(BeFalse())
		Expect(attachment.DeliveryPrompt.InPrompt()).To(BeTrue())
		Expect(attachment.DeliveryBoth.InPrompt()).To(BeTrue())
	})

	It("parses the names the CLI accepts", func() {
		for name, want := range map[string]attachment.Delivery{
			"": attachment.DeliveryAuto, "auto": attachment.DeliveryAuto, "vm": attachment.DeliveryVM,
			"prompt": attachment.DeliveryPrompt, "both": attachment.DeliveryBoth,
		} {
			got, err := attachment.ParseDelivery(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		}
		_, err := attachment.ParseDelivery("disk")
		Expect(err).To(MatchError(ContainSubstring("use auto, vm, prompt or both")))
	})
})

var _ = Describe("DetectMediaType", func() {
	It("returns the base type of the content", func() {
		Expect(attachment.DetectMediaType(pngBytes())).To(Equal("image/png"))
		Expect(attachment.DetectMediaType([]byte("hello"))).To(Equal("text/plain"))
	})
})

var _ = Describe("ValidateName", func() {
	DescribeTable("accepts a single file name",
		func(name string) { Expect(attachment.ValidateName(name)).To(Succeed()) },
		Entry("plain", "spec.md"),
		Entry("hidden", ".env.example"),
		Entry("spaces and unicode", "skärmbild 1.png"),
		Entry("at the length limit", strings.Repeat("a", 255)),
	)

	DescribeTable("refuses anything else",
		func(name, wantErr string) {
			Expect(attachment.ValidateName(name)).To(MatchError(ContainSubstring(wantErr)))
		},
		Entry("empty", "", "empty"),
		Entry("current directory", ".", "not a file name"),
		Entry("parent directory", "..", "not a file name"),
		Entry("slash", "dir/a.txt", "path separator"),
		Entry("backslash", `dir\a.txt`, "path separator"),
		Entry("NUL", "a\x00b", "control character"),
		Entry("newline", "a\nb", "control character"),
		Entry("invalid UTF-8", "a\xffb", "not valid UTF-8"),
		Entry("over the length limit", strings.Repeat("a", 256), "the limit is 255"),
	)
})

var _ = Describe("ReadArgs", func() {
	It("reads files named by their base names, with their deliveries", func() {
		dir := GinkgoT().TempDir()
		p := filepath.Join(dir, "notes.md")
		Expect(os.WriteFile(p, []byte("hello"), 0o600)).To(Succeed())

		got, err := attachment.ReadArgs([]string{p, "prompt=" + p})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal([]attachment.File{
			{Name: "notes.md", Data: []byte("hello")},
			{Name: "notes.md", Delivery: attachment.DeliveryPrompt, Data: []byte("hello")},
		}))
	})

	It("refuses a directory", func() {
		_, err := attachment.ReadArgs([]string{GinkgoT().TempDir()})
		Expect(err).To(MatchError(ContainSubstring("not a regular file")))
	})

	It("refuses a file over the size limit before reading it", func() {
		p := filepath.Join(GinkgoT().TempDir(), "big.bin")
		f, err := os.Create(p)
		Expect(err).NotTo(HaveOccurred())
		Expect(f.Truncate(attachment.MaxFileBytes + 1)).To(Succeed())
		Expect(f.Close()).To(Succeed())

		_, err = attachment.ReadArgs([]string{p})
		Expect(err).To(MatchError(ContainSubstring("the limit is 10485760")))
	})
})
