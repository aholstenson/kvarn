package coding

import (
	llms "github.com/aholstenson/llms-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/attachment"
)

var _ = Describe("firstMessage", func() {
	It("puts the files before the text, each under its name", func() {
		msg := firstMessage("look at these", []attachment.File{
			{Name: "a.png", MediaType: "image/png", Data: []byte("a")},
			{Name: "spec.md", MediaType: "text/plain", Data: []byte("b")},
		})
		Expect(msg.Role).To(Equal(llms.RoleUser))
		Expect(msg.Parts).To(HaveLen(3))
		Expect(msg.Parts[0]).To(Equal(llms.NewBinaryPart("image/png", []byte("a")).WithName("a.png")))
		Expect(msg.Parts[1]).To(Equal(llms.NewBinaryPart("text/plain", []byte("b")).WithName("spec.md")))
		Expect(msg.Parts[2]).To(Equal(llms.NewTextPart("look at these")))
	})

	It("is the text alone without files", func() {
		msg := firstMessage("do it", nil)
		Expect(msg.Parts).To(Equal([]llms.MessagePart{llms.NewTextPart("do it")}))
	})
})
