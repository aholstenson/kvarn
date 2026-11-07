package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"
	llms "github.com/aholstenson/llms-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/attachment"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/sandbox"
	"github.com/aholstenson/kvarn/internal/session"
)

var _ = Describe("Job attachments", func() {
	var (
		ctx context.Context
		svc *Service
		mgr session.Manager
	)

	// As in the idempotency specs: no dispatcher runs, so a submission stays in
	// the backlog and the assertions are about what StartJob recorded.
	BeforeEach(func() {
		ctx = context.Background()
		mgr = session.NewManager(session.NewMemStore())
		svc = &Service{
			sessionMgr: mgr,
			projectStore: &idempotencyProjectStore{proj: &project.Project{
				Name:          "alpha",
				RepoURL:       "/nonexistent/repo.git",
				DefaultBranch: "main",
				Forge:         "test-forge",
			}},
		}
		svc.dispatcher = newDispatcher(svc, DispatchPolicy{})
	})

	start := func(key string, attachments ...*v1.Attachment) (*v1.StartJobResponse, error) {
		return svc.StartJob(ctx, &v1.StartJobRequest{
			Project:        "alpha",
			Prompt:         "match the mockup",
			IdempotencyKey: key,
			Attachments:    attachments,
		})
	}

	spec := []byte("# Spec\n\nThe button is blue.\n")

	It("stores the attachments and describes them on the session", func() {
		resp, err := start("", &v1.Attachment{Name: "spec.md", Data: spec},
			&v1.Attachment{Name: "data.json", MediaType: "application/json", Data: []byte(`{}`),
				Delivery: v1.AttachmentDelivery_ATTACHMENT_DELIVERY_VM})
		Expect(err).NotTo(HaveOccurred())

		got, err := svc.GetSession(ctx, &v1.GetSessionRequest{SessionId: resp.SessionId})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Attachments).To(HaveLen(2))
		Expect(got.Attachments[0].Name).To(Equal("spec.md"))
		Expect(got.Attachments[0].MediaType).To(Equal("text/plain; charset=utf-8"))
		Expect(got.Attachments[0].Size).To(Equal(int64(len(spec))))
		Expect(got.Attachments[0].Sha256).To(Equal(attachment.Digest(spec)))
		Expect(got.Attachments[0].Delivery).To(Equal(v1.AttachmentDelivery_ATTACHMENT_DELIVERY_UNSPECIFIED))
		Expect(got.Attachments[1].Name).To(Equal("data.json"))
		Expect(got.Attachments[1].MediaType).To(Equal("application/json"))
		Expect(got.Attachments[1].Delivery).To(Equal(v1.AttachmentDelivery_ATTACHMENT_DELIVERY_VM))

		content, err := mgr.AttachmentData(ctx, attachment.Digest(spec))
		Expect(err).NotTo(HaveOccurred())
		Expect(content).To(Equal(spec))
	})

	DescribeTable("refuses a submission it cannot carry out as asked",
		func(a *v1.Attachment, wantErr string) {
			_, err := start("", a)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err).To(MatchError(ContainSubstring(wantErr)))
		},
		Entry("a name with a path", &v1.Attachment{Name: "../escape.txt", Data: spec}, "path separator"),
		Entry("a binary file asked into the prompt", &v1.Attachment{Name: "data.bin", Data: []byte{0, 1, 0xff},
			Delivery: v1.AttachmentDelivery_ATTACHMENT_DELIVERY_PROMPT}, "cannot go into the prompt"),
		Entry("an unknown delivery", &v1.Attachment{Name: "spec.md", Data: spec,
			Delivery: v1.AttachmentDelivery(99)}, "unknown delivery"),
		Entry("a mislabelled image", &v1.Attachment{Name: "shot.png", MediaType: "image/png", Data: spec},
			"declared as image/png"),
	)

	Describe("files asked into the prompt", func() {
		var planner *planningAgent

		BeforeEach(func() {
			planner = &planningAgent{}
			svc.agent = planner
		})

		prompted := func(name string) *v1.Attachment {
			return &v1.Attachment{Name: name, Data: spec, Delivery: v1.AttachmentDelivery_ATTACHMENT_DELIVERY_PROMPT}
		}

		It("asks the agent about them alone, and stores them when it accepts", func() {
			resp, err := start("", prompted("spec.md"), &v1.Attachment{Name: "auto.md", Data: spec})
			Expect(err).NotTo(HaveOccurred())

			Expect(planner.asked).To(HaveLen(1))
			Expect(planner.asked[0].Name).To(Equal("spec.md"))

			got, err := svc.GetSession(ctx, &v1.GetSessionRequest{SessionId: resp.SessionId})
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Attachments[0].Delivery).To(Equal(v1.AttachmentDelivery_ATTACHMENT_DELIVERY_PROMPT))
		})

		It("does not ask the agent when nothing asks for the prompt", func() {
			_, err := start("", &v1.Attachment{Name: "auto.md", Data: spec})
			Expect(err).NotTo(HaveOccurred())
			Expect(planner.asked).To(BeNil())
		})

		It("refuses a file the model does not accept as the submitter's error", func() {
			planner.err = fmt.Errorf("attachment %q cannot go into the prompt: %w", "spec.md",
				&llms.InputError{Model: "m", Detail: "model does not support image input", Err: llms.ErrUnsupportedModality})
			_, err := start("", prompted("spec.md"))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err).To(MatchError(ContainSubstring("model does not support image input")))
		})

		It("reports a model configuration that does not load as unavailable", func() {
			planner.err = errors.New("resolve agent models: agents.toml: syntax error")
			_, err := start("", prompted("spec.md"))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
		})
	})

	It("writes no session for a refused submission", func() {
		_, err := start("", &v1.Attachment{Name: "a/b", Data: spec})
		Expect(err).To(HaveOccurred())

		all, err := mgr.List(ctx, session.SessionFilter{})
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(BeEmpty())
	})

	Describe("idempotency", func() {
		It("replays a submission with the same attachments", func() {
			first, err := start("req-1", &v1.Attachment{Name: "spec.md", Data: spec})
			Expect(err).NotTo(HaveOccurred())

			second, err := start("req-1", &v1.Attachment{Name: "spec.md", Data: spec})
			Expect(err).NotTo(HaveOccurred())
			Expect(second.Duplicate).To(BeTrue())
			Expect(second.SessionId).To(Equal(first.SessionId))
		})

		DescribeTable("treats different attachments as a different submission",
			func(changed ...*v1.Attachment) {
				_, err := start("req-1", &v1.Attachment{Name: "spec.md", Data: spec})
				Expect(err).NotTo(HaveOccurred())

				_, err = start("req-1", changed...)
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeAlreadyExists))
			},
			Entry("other content", &v1.Attachment{Name: "spec.md", Data: []byte("other")}),
			Entry("another name", &v1.Attachment{Name: "spec.txt", Data: spec}),
			Entry("another delivery", &v1.Attachment{Name: "spec.md", Data: spec,
				Delivery: v1.AttachmentDelivery_ATTACHMENT_DELIVERY_VM}),
			Entry("none at all"),
		)
	})
})

var _ = Describe("attachmentRefs", func() {
	It("gives every file delivered to the VM its path there", func() {
		refs := attachmentRefs([]session.Attachment{
			{Name: "auto.txt", MediaType: "text/plain", Size: 3},
			{Name: "vm.txt", MediaType: "text/plain", Size: 4, Delivery: attachment.DeliveryVM},
			{Name: "prompt.png", MediaType: "image/png", Size: 5, Delivery: attachment.DeliveryPrompt},
		}, []attachment.File{{Name: "prompt.png"}})
		Expect(refs).To(HaveLen(3))
		Expect(refs[0].Path).To(Equal(sandbox.AttachmentsDir + "/auto.txt"))
		Expect(refs[0].InPrompt).To(BeFalse())
		Expect(refs[1].Path).To(Equal(sandbox.AttachmentsDir + "/vm.txt"))
		Expect(refs[2].Path).To(BeEmpty())
		Expect(refs[2].InPrompt).To(BeTrue())
	})
})

// planningAgent records what it is asked to plan and accepts it all, unless
// err is set. It never runs: these specs stop at submission.
type planningAgent struct {
	agent.NoopAgent
	asked []attachment.File
	err   error
}

func (a *planningAgent) PlanPromptAttachments(_ context.Context, candidates []attachment.File) ([]attachment.File, error) {
	a.asked = append(a.asked, candidates...)
	if a.err != nil {
		return nil, a.err
	}
	return candidates, nil
}
