package job

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/attachment"
)

var _ = Describe("planPromptAttachments", func() {
	files := []attachment.File{
		{Name: "shot.png", MediaType: "image/png"},
		{Name: "brief.md", MediaType: "text/plain", Delivery: attachment.DeliveryPrompt},
		{Name: "notes.md", MediaType: "text/plain"},
	}

	It("gives an agent that cannot plan only the files asked into the prompt", func() {
		got, err := planPromptAttachments(context.Background(), &agent.NoopAgent{}, files)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(got[0].Name).To(Equal("brief.md"))
	})

	It("lists files in the VM with their paths and marks those in the message", func() {
		refs := attachmentRefs([]attachment.File{
			{Name: "shot.png", MediaType: "image/png", Data: []byte("x")},
			{Name: "brief.md", MediaType: "text/plain", Delivery: attachment.DeliveryPrompt, Data: []byte("yy")},
		}, []attachment.File{{Name: "brief.md"}})
		Expect(refs[0].Path).To(Equal("/home/kvarn/attachments/shot.png"))
		Expect(refs[0].InPrompt).To(BeFalse())
		Expect(refs[1].Path).To(BeEmpty())
		Expect(refs[1].InPrompt).To(BeTrue())
		Expect(refs[1].Size).To(Equal(int64(2)))
	})
})
