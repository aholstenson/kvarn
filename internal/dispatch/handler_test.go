package dispatch_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/dispatch"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

var _ = Describe("Handler streaming", func() {
	var (
		registry *dispatch.Registry
		client   kvarnv1connect.BridgeServiceClient
		listener net.Listener
		server   *http.Server
	)

	BeforeEach(func() {
		registry = dispatch.NewRegistry()
		handler := dispatch.NewHandler(registry)

		var err error
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())

		mux := http.NewServeMux()
		path, h := kvarnv1connect.NewBridgeServiceHandler(handler)
		mux.Handle(path, h)

		server = &http.Server{
			Handler: h2c.NewHandler(mux, &http2.Server{}),
		}
		go server.Serve(listener)

		client = kvarnv1connect.NewBridgeServiceClient(
			http.DefaultClient,
			fmt.Sprintf("http://%s", listener.Addr().String()),
		)
	})

	AfterEach(func() {
		server.Close()
	})

	Describe("DownloadFile", func() {
		It("streams file data", func() {
			pr, err := registry.Register("dl-token")
			Expect(err).NotTo(HaveOccurred())

			content := "hello world streaming download"
			t := &dispatch.PendingTransfer{
				Reader: io.NopCloser(strings.NewReader(content)),
				Meta: &v1.FileStreamStart{
					TransferId: "dl-1",
					Path:       "/tmp/test.tar",
					Size:       int64(len(content)),
					Mode:       0o644,
				},
				Done: make(chan struct{}),
			}
			pr.RegisterTransfer("dl-1", t)

			stream, err := client.DownloadFile(context.Background(), connect.NewRequest(&v1.DownloadFileRequest{
				TransferId: "dl-1",
				Token:      "dl-token",
			}))
			Expect(err).NotTo(HaveOccurred())
			defer stream.Close()

			// First chunk should be start metadata.
			Expect(stream.Receive()).To(BeTrue())
			start := stream.Msg().GetStart()
			Expect(start).NotTo(BeNil())
			Expect(start.Path).To(Equal("/tmp/test.tar"))
			Expect(start.Size).To(Equal(int64(len(content))))

			// Subsequent chunks should be data.
			var received []byte
			for stream.Receive() {
				data := stream.Msg().GetData()
				Expect(data).NotTo(BeNil())
				received = append(received, data...)
			}
			Expect(stream.Err()).NotTo(HaveOccurred())
			Expect(string(received)).To(Equal(content))
		})

		It("returns error for unknown token", func() {
			stream, err := client.DownloadFile(context.Background(), connect.NewRequest(&v1.DownloadFileRequest{
				TransferId: "dl-1",
				Token:      "nonexistent",
			}))
			if err != nil {
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
			} else {
				// Error may come on first Receive for server-streaming RPCs.
				defer stream.Close()
				Expect(stream.Receive()).To(BeFalse())
				Expect(stream.Err()).To(HaveOccurred())
				Expect(connect.CodeOf(stream.Err())).To(Equal(connect.CodeNotFound))
			}
		})

		It("returns error for unknown transfer ID", func() {
			_, err := registry.Register("dl-token-2")
			Expect(err).NotTo(HaveOccurred())

			stream, err := client.DownloadFile(context.Background(), connect.NewRequest(&v1.DownloadFileRequest{
				TransferId: "nonexistent",
				Token:      "dl-token-2",
			}))
			if err != nil {
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
			} else {
				defer stream.Close()
				Expect(stream.Receive()).To(BeFalse())
				Expect(stream.Err()).To(HaveOccurred())
				Expect(connect.CodeOf(stream.Err())).To(Equal(connect.CodeNotFound))
			}
		})

		It("handles large file chunking", func() {
			pr, err := registry.Register("dl-token-3")
			Expect(err).NotTo(HaveOccurred())

			// Create data larger than one chunk (512KB).
			bigData := strings.Repeat("x", 1024*1024)
			t := &dispatch.PendingTransfer{
				Reader: io.NopCloser(strings.NewReader(bigData)),
				Meta: &v1.FileStreamStart{
					TransferId: "dl-big",
					Path:       "/tmp/big.tar",
					Size:       int64(len(bigData)),
				},
				Done: make(chan struct{}),
			}
			pr.RegisterTransfer("dl-big", t)

			stream, err := client.DownloadFile(context.Background(), connect.NewRequest(&v1.DownloadFileRequest{
				TransferId: "dl-big",
				Token:      "dl-token-3",
			}))
			Expect(err).NotTo(HaveOccurred())
			defer stream.Close()

			// Skip start.
			Expect(stream.Receive()).To(BeTrue())
			Expect(stream.Msg().GetStart()).NotTo(BeNil())

			// Count data chunks — should be more than one.
			chunkCount := 0
			var received []byte
			for stream.Receive() {
				data := stream.Msg().GetData()
				if data != nil {
					chunkCount++
					received = append(received, data...)
				}
			}
			Expect(stream.Err()).NotTo(HaveOccurred())
			Expect(chunkCount).To(BeNumerically(">", 1))
			Expect(string(received)).To(Equal(bigData))
		})

		It("propagates reader errors", func() {
			pr, err := registry.Register("dl-token-4")
			Expect(err).NotTo(HaveOccurred())

			t := &dispatch.PendingTransfer{
				Reader: io.NopCloser(&errorReader{err: fmt.Errorf("disk read error"), afterBytes: 10}),
				Meta: &v1.FileStreamStart{
					TransferId: "dl-err",
					Path:       "/tmp/err.tar",
				},
				Done: make(chan struct{}),
			}
			pr.RegisterTransfer("dl-err", t)

			stream, err := client.DownloadFile(context.Background(), connect.NewRequest(&v1.DownloadFileRequest{
				TransferId: "dl-err",
				Token:      "dl-token-4",
			}))
			Expect(err).NotTo(HaveOccurred())
			defer stream.Close()

			// Should get start, then an error during data.
			Expect(stream.Receive()).To(BeTrue()) // start
			// Drain — eventually should see the error.
			for stream.Receive() {
			}
			Expect(stream.Err()).To(HaveOccurred())
		})
	})

	Describe("UploadFile", func() {
		It("receives file data", func() {
			pr, err := registry.Register("ul-token")
			Expect(err).NotTo(HaveOccurred())

			pr2, pw := io.Pipe()
			t := &dispatch.PendingTransfer{
				Writer: pw,
				Meta: &v1.FileStreamStart{
					TransferId: "ul-1",
					Path:       "/tmp/upload.tar",
				},
				Done: make(chan struct{}),
			}
			pr.RegisterTransfer("ul-1", t)

			// Read from pipe in background.
			receivedCh := make(chan string, 1)
			go func() {
				data, _ := io.ReadAll(pr2)
				receivedCh <- string(data)
			}()

			stream := client.UploadFile(context.Background())
			err = stream.Send(&v1.FileStreamChunk{
				Payload: &v1.FileStreamChunk_Start{Start: &v1.FileStreamStart{
					TransferId: "ul-1",
					Token:      "ul-token",
					Path:       "/tmp/upload.tar",
				}},
			})
			Expect(err).NotTo(HaveOccurred())

			content := "uploaded data"
			err = stream.Send(&v1.FileStreamChunk{
				Payload: &v1.FileStreamChunk_Data{Data: []byte(content)},
			})
			Expect(err).NotTo(HaveOccurred())

			resp, err := stream.CloseAndReceive()
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.BytesWritten).To(Equal(int64(len(content))))

			Eventually(receivedCh).Should(Receive(Equal(content)))
		})

		It("rejects first chunk without start metadata", func() {
			stream := client.UploadFile(context.Background())
			err := stream.Send(&v1.FileStreamChunk{
				Payload: &v1.FileStreamChunk_Data{Data: []byte("data")},
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = stream.CloseAndReceive()
			Expect(err).To(HaveOccurred())
		})

		It("returns error for unknown token in start", func() {
			stream := client.UploadFile(context.Background())
			err := stream.Send(&v1.FileStreamChunk{
				Payload: &v1.FileStreamChunk_Start{Start: &v1.FileStreamStart{
					TransferId: "ul-x",
					Token:      "nonexistent",
				}},
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = stream.CloseAndReceive()
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		})
	})
})

// errorReader returns afterBytes bytes of 'x' then returns the given error.
type errorReader struct {
	err        error
	afterBytes int
	read       int
}

func (r *errorReader) Read(p []byte) (int, error) {
	if r.read >= r.afterBytes {
		return 0, r.err
	}
	n := len(p)
	remaining := r.afterBytes - r.read
	if n > remaining {
		n = remaining
	}
	for i := range n {
		p[i] = 'x'
	}
	r.read += n
	return n, nil
}
