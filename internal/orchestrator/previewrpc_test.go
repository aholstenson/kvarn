package orchestrator

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/preview"
	projconfig "github.com/aholstenson/kvarn/internal/project"
)

var _ = Describe("Preview RPCs", func() {
	var (
		ctx    context.Context
		svc    *Service
		store  preview.Store
		client kvarnv1connect.OrchestratorServiceClient
		booted chan string
	)

	// serve stands the service up behind a real ConnectRPC client, so the specs
	// exercise the same encoding and error codes a CLI would see.
	serve := func(s *Service) kvarnv1connect.OrchestratorServiceClient {
		GinkgoHelper()
		mux := http.NewServeMux()
		orchestratorRPC := connect.NewServer()
		kvarnv1connect.RegisterOrchestratorServiceHandler(orchestratorRPC, s)
		connecthttp.Mount(mux, orchestratorRPC)
		server := httptest.NewServer(h2c.NewHandler(mux, &http2.Server{}))
		DeferCleanup(server.Close)
		return kvarnv1connect.NewOrchestratorServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL, connecthttp.WithReadMaxBytes(0))))
	}

	BeforeEach(func() {
		ctx = context.Background()
		store = preview.NewMemStore()
		DeferCleanup(func() { Expect(store.Close()).To(Succeed()) })
		booted = make(chan string, 16)

		svc = NewServiceWithOpts(ServiceOpts{
			PreviewStore:  store,
			PreviewPolicy: PreviewPolicy{Domain: "preview.example.com"},
		})
		svc.previews.boot = func(_ context.Context, p *preview.Preview, _ *preview.LogBuffer) (*previewBoot, error) {
			booted <- p.ID
			return &previewBoot{
				Sandbox: &dialSandbox{},
				Sites: []preview.Site{
					{Name: "web", Host: projconfig.RefLabel(p.Ref) + ".preview.example.com", Port: 3000},
				},
			}, nil
		}
		client = serve(svc)
	})

	Describe("StartPreview", func() {
		It("registers and boots a preview", func() {
			resp, err := client.StartPreview(ctx, &v1.StartPreviewRequest{
				Project: "proj",
				Ref:     "main",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Preview.Id).To(Equal("proj/main"))
			Expect(resp.Preview.Project).To(Equal("proj"))
			Expect(resp.Preview.Ref).To(Equal("main"))
			Eventually(booted).Should(Receive(Equal("proj/main")))
		})

		It("waits for the boot to settle when asked", func() {
			resp, err := client.StartPreview(ctx, &v1.StartPreviewRequest{
				Project: "proj", Ref: "main", Wait: true,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Preview.State).To(Equal(string(preview.StateRunning)))
			Expect(resp.Preview.Sites).To(HaveLen(1))
			Expect(resp.Preview.Sites[0].Url).To(HavePrefix("https://"))
			Expect(resp.Preview.Url).NotTo(BeEmpty())
		})

		It("returns the existing preview for a ref that already has one", func() {
			first, err := client.StartPreview(ctx, &v1.StartPreviewRequest{
				Project: "proj", Ref: "main", Wait: true,
			})
			Expect(err).NotTo(HaveOccurred())

			second, err := client.StartPreview(ctx, &v1.StartPreviewRequest{
				Project: "proj", Ref: "main", Wait: true,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(second.Preview.Id).To(Equal(first.Preview.Id))
			Expect(second.Preview.CreatedAt.AsTime()).
				To(BeTemporally("==", first.Preview.CreatedAt.AsTime()))
		})

		It("requires a project and a ref", func() {
			_, err := client.StartPreview(ctx, &v1.StartPreviewRequest{Ref: "main"})
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))

			_, err = client.StartPreview(ctx, &v1.StartPreviewRequest{Project: "proj"})
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})
	})

	Describe("GetPreview", func() {
		It("returns the preview and its retained output", func() {
			svc.previews.boot = func(_ context.Context, p *preview.Preview, logs *preview.LogBuffer) (*previewBoot, error) {
				logs.Append("listening on :3000\n")
				return &previewBoot{
					Sandbox: &dialSandbox{},
					Sites:   []preview.Site{{Name: "web", Host: projconfig.RefLabel(p.Ref) + ".preview.example.com", Port: 3000}},
				}, nil
			}

			_, err := client.StartPreview(ctx, &v1.StartPreviewRequest{
				Project: "proj", Ref: "main", Wait: true,
			})
			Expect(err).NotTo(HaveOccurred())

			resp, err := client.GetPreview(ctx, &v1.GetPreviewRequest{
				Project: "proj", Ref: "main",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Preview.State).To(Equal(string(preview.StateRunning)))
			Expect(resp.Logs).To(ContainSubstring("listening on :3000"))
		})

		It("reports not-found for a ref with no preview", func() {
			_, err := client.GetPreview(ctx, &v1.GetPreviewRequest{
				Project: "proj", Ref: "nope",
			})
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		})
	})

	Describe("ListPreviews", func() {
		It("lists every preview, and filters by project", func() {
			for _, target := range [][2]string{{"proj", "a"}, {"proj", "b"}, {"other", "c"}} {
				_, err := client.StartPreview(ctx, &v1.StartPreviewRequest{
					Project: target[0], Ref: target[1], Wait: true,
				})
				Expect(err).NotTo(HaveOccurred())
			}

			all, err := client.ListPreviews(ctx, &v1.ListPreviewsRequest{})
			Expect(err).NotTo(HaveOccurred())
			Expect(all.Previews).To(HaveLen(3))

			filtered, err := client.ListPreviews(ctx, &v1.ListPreviewsRequest{
				Project: "proj",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(filtered.Previews).To(HaveLen(2))
			for _, p := range filtered.Previews {
				Expect(p.Project).To(Equal("proj"))
			}
		})

		It("returns an empty list rather than an error when there is nothing", func() {
			resp, err := client.ListPreviews(ctx, &v1.ListPreviewsRequest{})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Previews).To(BeEmpty())
		})
	})

	Describe("StopPreview", func() {
		It("stops a preview but keeps it registered", func() {
			_, err := client.StartPreview(ctx, &v1.StartPreviewRequest{
				Project: "proj", Ref: "main", Wait: true,
			})
			Expect(err).NotTo(HaveOccurred())

			resp, err := client.StopPreview(ctx, &v1.StopPreviewRequest{
				Project: "proj", Ref: "main",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Preview.State).To(Equal(string(preview.StateStopped)))

			// Still listed, so the next request boots it again.
			list, err := client.ListPreviews(ctx, &v1.ListPreviewsRequest{})
			Expect(err).NotTo(HaveOccurred())
			Expect(list.Previews).To(HaveLen(1))
		})

		It("removes a preview entirely when asked", func() {
			_, err := client.StartPreview(ctx, &v1.StartPreviewRequest{
				Project: "proj", Ref: "main", Wait: true,
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = client.StopPreview(ctx, &v1.StopPreviewRequest{
				Project: "proj", Ref: "main", Remove: true,
			})
			Expect(err).NotTo(HaveOccurred())

			list, err := client.ListPreviews(ctx, &v1.ListPreviewsRequest{})
			Expect(err).NotTo(HaveOccurred())
			Expect(list.Previews).To(BeEmpty())
		})

		It("reports not-found for a ref with no preview", func() {
			_, err := client.StopPreview(ctx, &v1.StopPreviewRequest{
				Project: "proj", Ref: "nope",
			})
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		})
	})

	Describe("WatchPreview", func() {
		It("streams until the preview settles", func() {
			_, err := client.StartPreview(ctx, &v1.StartPreviewRequest{
				Project: "proj", Ref: "main", Wait: true,
			})
			Expect(err).NotTo(HaveOccurred())

			watchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			stream, err := client.WatchPreview(watchCtx, &v1.WatchPreviewRequest{
				Project: "proj", Ref: "main",
			})
			Expect(err).NotTo(HaveOccurred())
			defer stream.Close()

			msg, err := stream.Receive()
			Expect(err).NotTo(HaveOccurred())
			Expect(msg.Preview.State).To(Equal(string(preview.StateRunning)))
			// A settled preview ends the stream rather than holding it open.
			_, err = stream.Receive()
			Expect(err).To(MatchError(io.EOF))
		})
	})

	Describe("when previews are not configured", func() {
		It("reports every preview RPC as unimplemented", func() {
			bare := serve(NewServiceWithOpts(ServiceOpts{}))

			_, err := bare.StartPreview(ctx, &v1.StartPreviewRequest{
				Project: "proj", Ref: "main",
			})
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnimplemented))

			_, err = bare.ListPreviews(ctx, &v1.ListPreviewsRequest{})
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnimplemented))

			_, err = bare.GetPreview(ctx, &v1.GetPreviewRequest{
				Project: "proj", Ref: "main",
			})
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnimplemented))
		})
	})
})
