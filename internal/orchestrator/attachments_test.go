package orchestrator_test

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/attachment"
	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/forge"
	"github.com/aholstenson/kvarn/internal/orchestrator"
	"github.com/aholstenson/kvarn/internal/runner"
	"github.com/aholstenson/kvarn/internal/sandbox"
	"github.com/aholstenson/kvarn/internal/session"
	"github.com/aholstenson/kvarn/internal/vm"
	llms "github.com/aholstenson/llms-go"
)

// promptRecordingAgent behaves as the no-op agent and keeps the task message
// and the files it was started with. It plans like a model that sees images
// and reads text: it takes every candidate except a GIF, which stands in for a
// file the model refuses.
type promptRecordingAgent struct {
	mu      sync.Mutex
	prompt  string
	files   []attachment.File
	planned [][]attachment.File
	// refuseFromCall, when set, refuses every file asked into the prompt
	// from that call of PlanPromptAttachments on, counting from 1.
	refuseFromCall int
}

func (a *promptRecordingAgent) Start(ctx context.Context, agentCtx *agent.Context) (agent.Conversation, error) {
	a.mu.Lock()
	a.prompt = agentCtx.Prompt
	a.files = agentCtx.PromptAttachments
	a.mu.Unlock()
	return (&agent.NoopAgent{}).Start(ctx, agentCtx)
}

func (a *promptRecordingAgent) PlanPromptAttachments(_ context.Context, candidates []attachment.File) ([]attachment.File, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.planned = append(a.planned, candidates)
	refuseAll := a.refuseFromCall > 0 && len(a.planned) >= a.refuseFromCall
	var out []attachment.File
	for _, f := range candidates {
		if f.MediaType == "image/gif" || refuseAll {
			if f.Delivery.InPrompt() {
				return nil, fmt.Errorf("attachment %q cannot go into the prompt: %w", f.Name,
					&llms.InputError{Model: "test", Detail: "refused", Err: llms.ErrUnsupportedMediaType})
			}
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

func (a *promptRecordingAgent) lastPrompt() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.prompt
}

func (a *promptRecordingAgent) promptFiles() []attachment.File {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.files
}

var _ = Describe("A run with attachments", func() {
	var (
		client      kvarnv1connect.OrchestratorServiceClient
		server      *http.Server
		sessionMgr  session.Manager
		tmpDir      string
		testAgent   *promptRecordingAgent
		capturedMu  sync.Mutex
		capturedOps []sandbox.Opts
	)

	BeforeEach(func() {
		sessionMgr = session.NewManager(session.NewMemStore())
		testAgent = &promptRecordingAgent{}
		capturedOps = nil

		var err error
		tmpDir, err = os.MkdirTemp("", "attachments-test-*")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { os.RemoveAll(tmpDir) })

		factory := func(_ context.Context, opts sandbox.Opts) (orchestrator.Sandbox, error) {
			capturedMu.Lock()
			capturedOps = append(capturedOps, opts)
			capturedMu.Unlock()

			wsDir, err := os.MkdirTemp("", "attachments-ws-*")
			if err != nil {
				return nil, err
			}
			if opts.SourceDir != "" {
				cp := exec.Command("cp", "-a", opts.SourceDir+"/.", wsDir)
				if out, err := cp.CombinedOutput(); err != nil {
					return nil, fmt.Errorf("copy source: %s: %w", out, err)
				}
			}
			h := runner.NewUnprivilegedHandler()
			proxy := &localRunnerProxy{handler: h}
			sessResp, err := proxy.CreateSession(context.Background(), &v1.CreateSessionRequest{WorkingDir: wsDir})
			if err != nil {
				return nil, err
			}
			return newTestSandbox(proxy, sessResp.SessionId, wsDir), nil
		}

		svc := orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{
			CreateOpts: vm.CreateOpts{},
			ProjectStore: &memProjectStore{projects: map[string]*project.Project{
				"test-project": {
					Name:          "test-project",
					RepoURL:       filepath.Join(tmpDir, "repo.git"),
					DefaultBranch: "master",
					Forge:         "test-forge",
				},
			}},
			ForgeConfigStore: &memForgeConfigStore{configs: map[string]*forgeconfig.ForgeConfig{
				"test-forge": {Name: "test-forge", Type: "mock"},
			}},
			ForgeTypes: map[string]forge.Forge{
				"mock": &mockForge{scmImpl: &mockSCM{files: map[string][]byte{"kvarn.yml": []byte("{}\n")}}},
			},
			SessionMgr:     sessionMgr,
			Agent:          testAgent,
			SandboxFactory: factory,
		})
		DeferCleanup(func() { svc.Shutdown(context.Background()) })

		listener, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		mux := http.NewServeMux()
		orchestratorRPC := connect.NewServer()
		kvarnv1connect.RegisterOrchestratorServiceHandler(orchestratorRPC, svc)
		connecthttp.Mount(mux, orchestratorRPC)
		bridgeRPC := connect.NewServer()
		kvarnv1connect.RegisterBridgeServiceHandler(bridgeRPC, svc.BridgeHandler())
		connecthttp.Mount(mux, bridgeRPC)
		server = &http.Server{Handler: mux}
		go server.Serve(listener)
		DeferCleanup(func() { server.Close() })

		client = kvarnv1connect.NewOrchestratorServiceClient(connect.NewClient(connecthttp.NewTransport(
			http.DefaultClient, fmt.Sprintf("http://%s", listener.Addr().String()), connecthttp.WithReadMaxBytes(0))))
	})

	It("writes the files into the VM and lists them in the task message", func() {
		spec := []byte("# Spec\n\nThe button is blue.\n")
		data := []byte(`{"color":"blue"}`)
		resp, err := client.StartJob(context.Background(), &v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "match the spec",
			Attachments: []*v1.Attachment{
				{Name: "spec.md", Data: spec},
				{Name: "colors.json", MediaType: "application/json", Data: data,
					Delivery: v1.AttachmentDelivery_ATTACHMENT_DELIVERY_VM},
			},
		})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() string {
			s, err := sessionMgr.Get(context.Background(), resp.SessionId)
			if err != nil {
				return ""
			}
			return string(s.State)
		}).Should(Equal("completed"))

		capturedMu.Lock()
		Expect(capturedOps).To(HaveLen(1))
		opts := capturedOps[0]
		capturedMu.Unlock()
		Expect(opts.Attachments).To(Equal([]attachment.File{
			{Name: "spec.md", MediaType: "text/plain; charset=utf-8", Data: spec},
			{Name: "colors.json", MediaType: "application/json", Delivery: attachment.DeliveryVM, Data: data},
		}))

		prompt := testAgent.lastPrompt()
		Expect(prompt).To(HavePrefix("match the spec"))
		Expect(prompt).To(ContainSubstring("## Attachments"))
		Expect(prompt).To(ContainSubstring("- /home/kvarn/attachments/spec.md (text/plain; charset=utf-8, 28B)"))
		Expect(prompt).To(ContainSubstring("- /home/kvarn/attachments/colors.json (application/json, 16B)"))
	})

	It("puts files into the first message as the plan decides, and says so", func() {
		shot := pngBytes()
		gif := []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;")
		brief := []byte("Use the brand colours.\n")
		resp, err := client.StartJob(context.Background(), &v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "match the mockup",
			Attachments: []*v1.Attachment{
				{Name: "mockup.png", Data: shot},
				{Name: "anim.gif", MediaType: "image/gif", Data: gif},
				{Name: "brief.md", Data: brief, Delivery: v1.AttachmentDelivery_ATTACHMENT_DELIVERY_PROMPT},
			},
		})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() string {
			s, err := sessionMgr.Get(context.Background(), resp.SessionId)
			if err != nil {
				return ""
			}
			return string(s.State)
		}).Should(Equal("completed"))

		// The image the model accepts and the text asked for go into the
		// message; the refused GIF does not, and does not fail the run.
		files := testAgent.promptFiles()
		Expect(files).To(HaveLen(2))
		Expect(files[0].Name).To(Equal("mockup.png"))
		Expect(files[0].Data).To(Equal(shot))
		Expect(files[1].Name).To(Equal("brief.md"))

		// A prompt-only file is not written into the VM.
		capturedMu.Lock()
		vmNames := []string{}
		for _, f := range capturedOps[0].Attachments {
			vmNames = append(vmNames, f.Name)
		}
		capturedMu.Unlock()
		Expect(vmNames).To(Equal([]string{"mockup.png", "anim.gif"}))

		prompt := testAgent.lastPrompt()
		Expect(prompt).To(ContainSubstring("- /home/kvarn/attachments/mockup.png (image/png, "))
		Expect(prompt).To(ContainSubstring("; also included in this message)"))
		Expect(prompt).To(ContainSubstring("- /home/kvarn/attachments/anim.gif (image/gif, 14B)\n"))
		Expect(prompt).To(ContainSubstring("- brief.md (text/plain; charset=utf-8, 23B; included in this message only, not in the VM)"))
	})

	It("refuses at submission a file asked into the prompt that the model refuses", func() {
		_, err := client.StartJob(context.Background(), &v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "look",
			Attachments: []*v1.Attachment{{Name: "anim.gif", MediaType: "image/gif",
				Data:     []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"),
				Delivery: v1.AttachmentDelivery_ATTACHMENT_DELIVERY_BOTH}},
		})
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(err).To(MatchError(ContainSubstring(`attachment "anim.gif" cannot go into the prompt`)))
	})

	It("fails the run before the VM boots when the model no longer accepts a file", func() {
		// The first plan is the check at submission, the second the run's.
		// Refusing only the second is what a change to agents.toml between
		// the two looks like.
		testAgent.refuseFromCall = 2
		resp, err := client.StartJob(context.Background(), &v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "look",
			Attachments: []*v1.Attachment{{Name: "brief.md", Data: []byte("brief\n"),
				Delivery: v1.AttachmentDelivery_ATTACHMENT_DELIVERY_PROMPT}},
		})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() string {
			s, err := sessionMgr.Get(context.Background(), resp.SessionId)
			if err != nil {
				return ""
			}
			return string(s.State)
		}).Should(Equal("failed"))
		s, err := sessionMgr.Get(context.Background(), resp.SessionId)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.Error).To(ContainSubstring(`attachment "brief.md" cannot go into the prompt`))

		capturedMu.Lock()
		defer capturedMu.Unlock()
		Expect(capturedOps).To(BeEmpty())
	})

	It("gives a run without attachments no attachment list", func() {
		resp, err := client.StartJob(context.Background(), &v1.StartJobRequest{
			Project: "test-project",
			Prompt:  "do the thing",
		})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() string {
			s, err := sessionMgr.Get(context.Background(), resp.SessionId)
			if err != nil {
				return ""
			}
			return string(s.State)
		}).Should(Equal("completed"))

		Expect(testAgent.lastPrompt()).To(Equal("do the thing"))
		capturedMu.Lock()
		defer capturedMu.Unlock()
		Expect(capturedOps[0].Attachments).To(BeEmpty())
	})
})

// pngBytes encodes a small real image.
func pngBytes() []byte {
	var buf bytes.Buffer
	Expect(png.Encode(&buf, image.NewGray(image.Rect(0, 0, 2, 2)))).To(Succeed())
	return buf.Bytes()
}
