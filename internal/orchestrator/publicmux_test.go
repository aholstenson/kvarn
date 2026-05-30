package orchestrator_test

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/orchestrator"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("PublicMux", func() {
	// BridgeService must never be reachable from the orchestrator's TCP
	// listener — it carries runner-impersonation primitives and is only safe
	// over the per-sandbox vsock transport set up in internal/sandbox.
	It("does not expose BridgeService over the public listener", func() {
		svc := orchestrator.NewServiceWithOpts(orchestrator.ServiceOpts{
			AuthEnabled: false,
		})

		listener, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		defer listener.Close()

		server := &http.Server{
			Handler: h2c.NewHandler(orchestrator.PublicMux(svc), &http2.Server{}),
		}
		go server.Serve(listener)
		defer server.Close()

		baseURL := fmt.Sprintf("http://%s", listener.Addr().String())

		bridge := kvarnv1connect.NewBridgeServiceClient(http.DefaultClient, baseURL)
		_, err = bridge.ReportResult(context.Background(), connect.NewRequest(&v1.CommandResult{Token: "anything"}))
		Expect(err).To(HaveOccurred())
		// http.ServeMux returns 404 for an unmounted path; connect-go maps
		// that to CodeUnimplemented, which is what we want here — anything
		// short of NotFound/Unimplemented would mean the bridge handler is
		// still reachable.
		Expect(connect.CodeOf(err)).To(SatisfyAny(
			Equal(connect.CodeUnimplemented),
			Equal(connect.CodeNotFound),
		))
	})
})
