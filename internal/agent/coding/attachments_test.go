package coding_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	llms "github.com/aholstenson/llms-go"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/agent/coding"
	"github.com/aholstenson/kvarn/internal/attachment"
	modeltoml "github.com/aholstenson/kvarn/internal/config/model/tomlstore"
)

// Models the input check knows, registered under names no real provider
// uses. The "openai/" prefix gives them a provider llms-go checks input for,
// and the static credential means nothing is fetched to build them.
const (
	textOnlyModel = "openai/kvarn-test-text-only"
	visionModel   = "openai/kvarn-test-vision"
	// visionMaxBytes is small enough that a test image can exceed it.
	visionMaxBytes = 2000
)

func init() {
	llms.RegisterModelInfo(textOnlyModel, llms.ModelInfo{
		Caps:       llms.Capabilities{ToolCall: true},
		Modalities: []string{"text"},
	})
	llms.RegisterModelInfo(visionModel, llms.ModelInfo{
		Caps:       llms.Capabilities{ToolCall: true, Attachment: true},
		Modalities: []string{"text", "image", "pdf"},
		Limits: llms.Limits{Image: llms.ImageLimits{
			MediaTypes: []string{"image/png", "image/jpeg"},
			MaxBytes:   visionMaxBytes,
		}},
	})
}

// testPNG encodes an image of the given size with random pixels, so a large
// one does not compress below visionMaxBytes.
func testPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	_, _ = rand.Read(img.Pix)
	var buf bytes.Buffer
	Expect(png.Encode(&buf, img)).To(Succeed())
	return buf.Bytes()
}

// newManager returns a manager whose models need no real API key.
func newManager() *llms.Manager {
	mgr, err := llms.NewManager(llms.WithManagerCredentials(llms.StaticCredentials("test-key")))
	Expect(err).NotTo(HaveOccurred())
	return mgr
}

// agentOn returns a coding agent whose main loop runs on model.
func agentOn(model string) *coding.CodingAgent {
	path := filepath.Join(GinkgoT().TempDir(), "agents.toml")
	Expect(os.WriteFile(path, []byte(`
[models.coding-agent-fast]
model = "test/fast"

[models.coding-agent-reasoning]
model = "test/reasoning"

[models.coding-agent]
model = "`+model+`"
`), 0o600)).To(Succeed())
	return coding.NewCodingAgent(coding.NewResolver(newManager(), modeltoml.New(path), ""))
}

var _ = Describe("CodingAgent.PlanPromptAttachments", func() {
	ctx := context.Background()
	smallPNG := testPNG(2, 2)
	text := []byte("# Spec\n")

	file := func(name, mediaType string, d attachment.Delivery, data []byte) attachment.File {
		return attachment.File{Name: name, MediaType: mediaType, Delivery: d, Data: data}
	}
	names := func(files []attachment.File) []string {
		out := []string{}
		for _, f := range files {
			out = append(out, f.Name)
		}
		return out
	}

	It("leaves out an automatic image the model cannot see, without failing", func() {
		got, err := agentOn(textOnlyModel).PlanPromptAttachments(ctx, []attachment.File{
			file("shot.png", "image/png", attachment.DeliveryAuto, smallPNG),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
	})

	It("fails for an image the submitter asked into the prompt of a model that cannot see", func() {
		_, err := agentOn(textOnlyModel).PlanPromptAttachments(ctx, []attachment.File{
			file("shot.png", "image/png", attachment.DeliveryPrompt, smallPNG),
		})
		Expect(err).To(MatchError(ContainSubstring(`attachment "shot.png" cannot go into the prompt`)))
		Expect(errors.Is(err, llms.ErrUnsupportedModality)).To(BeTrue())
		var inputErr *llms.InputError
		Expect(errors.As(err, &inputErr)).To(BeTrue())
	})

	It("puts a text file into the prompt of any model", func() {
		got, err := agentOn(textOnlyModel).PlanPromptAttachments(ctx, []attachment.File{
			file("spec.md", "text/plain; charset=utf-8", attachment.DeliveryBoth, text),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(names(got)).To(Equal([]string{"spec.md"}))
	})

	It("keeps the images a seeing model accepts and drops an automatic one over its limit", func() {
		big := testPNG(64, 64)
		Expect(len(big)).To(BeNumerically(">", visionMaxBytes))
		got, err := agentOn(visionModel).PlanPromptAttachments(ctx, []attachment.File{
			file("small.png", "image/png", attachment.DeliveryAuto, smallPNG),
			file("big.png", "image/png", attachment.DeliveryAuto, big),
			file("spec.md", "text/plain", attachment.DeliveryPrompt, text),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(names(got)).To(Equal([]string{"small.png", "spec.md"}))
	})

	It("fails for an image over the limit the submitter asked for", func() {
		_, err := agentOn(visionModel).PlanPromptAttachments(ctx, []attachment.File{
			file("big.png", "image/png", attachment.DeliveryBoth, testPNG(64, 64)),
		})
		Expect(errors.Is(err, llms.ErrAttachmentTooLarge)).To(BeTrue())
	})

	It("does nothing without candidates", func() {
		got, err := agentOn(textOnlyModel).PlanPromptAttachments(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeNil())
	})
})

var _ = Describe("view_file", func() {
	var (
		ctx     context.Context
		runner  *mockRunner
		pngData []byte
	)

	tool := func(model llms.Model) llms.ToolDef {
		tk := coding.NewCodingToolkitWithOpts(coding.CodingToolkitOpts{
			Runner:     runner,
			WorkingDir: "/home/kvarn/workspace",
			SessionID:  "s",
			ViewModel:  model,
		})
		for _, td := range tk.Tools() {
			if td.Name() == "view_file" {
				return td
			}
		}
		Fail("view_file is not in the toolkit")
		return nil
	}

	serve := func(data []byte) {
		runner.execFunc = func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
			// The file is read as the job's user, so the tool can reach no
			// more than the agent's own shell commands can.
			Expect(req.Privileged).To(BeFalse())
			Expect(req.Command).To(Equal("sh"))
			Expect(req.WorkingDir).To(Equal("/home/kvarn/workspace"))
			Expect(req.Args[3]).To(Equal("/home/kvarn/scratch/shot.png"))
			Expect(req.Args[4]).To(Equal("10485760"))
			// Wrapped output, the way base64 prints it.
			enc := base64.StdEncoding.EncodeToString(data)
			wrapped := ""
			for len(enc) > 76 {
				wrapped += enc[:76] + "\n"
				enc = enc[76:]
			}
			return &v1.ExecResponse{Stdout: wrapped + enc + "\n"}, nil
		}
	}

	BeforeEach(func() {
		ctx = context.Background()
		runner = &mockRunner{}
		pngData = testPNG(4, 4)
	})

	It("is offered in read-only modes too", func() {
		tk := coding.NewCodingToolkit(runner, "/home/kvarn/workspace", "s", nil)
		var names []string
		for _, td := range tk.ReadOnlyTools() {
			names = append(names, td.Name())
		}
		Expect(names).To(ContainElement("view_file"))
	})

	It("adds an image to the conversation under its file name", func() {
		serve(pngData)
		td := tool(nil)
		out, err := td.Execute(ctx, &coding.ViewFileInput{Path: "/home/kvarn/scratch/shot.png"})
		Expect(err).NotTo(HaveOccurred())

		res := td.Render(out)
		Expect(res.Text).To(Equal(fmt.Sprintf("Showing /home/kvarn/scratch/shot.png (image/png, %dB).", len(pngData))))
		Expect(res.Attachments).To(HaveLen(1))
		part, ok := res.Attachments[0].(*llms.BinaryPart)
		Expect(ok).To(BeTrue())
		Expect(part.MediaType).To(Equal("image/png"))
		Expect(part.Name).To(Equal("shot.png"))
		Expect(part.Data).To(Equal(pngData))
	})

	It("refuses text and points at read_file", func() {
		serve([]byte("plain text\n"))
		_, err := tool(nil).Execute(ctx, &coding.ViewFileInput{Path: "/home/kvarn/scratch/shot.png"})
		Expect(err).To(MatchError(ContainSubstring("read_file reads text")))
	})

	It("refuses an image the model cannot see", func() {
		serve(pngData)
		model, err := newManager().GetModel(ctx, textOnlyModel)
		Expect(err).NotTo(HaveOccurred())
		_, err = tool(model).Execute(ctx, &coding.ViewFileInput{Path: "/home/kvarn/scratch/shot.png"})
		Expect(errors.Is(err, llms.ErrUnsupportedModality)).To(BeTrue())
	})

	It("reports why the guest could not read the file", func() {
		runner.execFunc = func(context.Context, *v1.ExecRequest) (*v1.ExecResponse, error) {
			return &v1.ExecResponse{ExitCode: 3, Stderr: "shot.png is 20000000 bytes; view_file reads at most 10485760\n"}, nil
		}
		_, err := tool(nil).Execute(ctx, &coding.ViewFileInput{Path: "shot.png"})
		Expect(err).To(MatchError("read shot.png: shot.png is 20000000 bytes; view_file reads at most 10485760"))
	})

	It("requires a path", func() {
		_, err := tool(nil).Execute(ctx, &coding.ViewFileInput{Path: " "})
		Expect(err).To(MatchError(ContainSubstring("path is required")))
	})
})
