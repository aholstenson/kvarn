package runner

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/dispatch"
	"github.com/aholstenson/kvarn/internal/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// The relay is what makes a preview reachable, so it is exercised end to end:
// a real bridge server, the real runner command loop, and a server that — like
// a published container port — listens only on loopback.
var _ = Describe("guest connection relay", func() {
	var (
		bridge   *http.Server
		listener net.Listener
		proxy    *sandbox.BridgeProxy
		cancel   context.CancelFunc
	)

	BeforeEach(func() {
		registry := dispatch.NewRegistry()
		pr, err := registry.Register("relay-token")
		Expect(err).NotTo(HaveOccurred())

		listener, err = net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())

		mux := http.NewServeMux()
		path, h := kvarnv1connect.NewBridgeServiceHandler(dispatch.NewHandler(registry))
		mux.Handle(path, h)
		bridge = &http.Server{Handler: h2c.NewHandler(mux, &http2.Server{})}
		go bridge.Serve(listener)

		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		go connectToOrchestrator(ctx, http.DefaultClient,
			fmt.Sprintf("http://%s", listener.Addr().String()), "relay-token")

		Eventually(pr.DoneCh, "5s").Should(BeClosed())
		proxy = sandbox.NewBridgeProxy(pr.CommandCh, pr.ResultCh, pr.OutputCh, pr)
	})

	AfterEach(func() {
		cancel()
		bridge.Close()
	})

	It("carries a request and its answer to a server on the guest's loopback", func() {
		guest := startEchoServer()
		defer guest.Close()

		ctx, cancelDial := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelDial()

		conn, err := proxy.DialGuest(ctx, addrPort(guest))
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close()

		_, err = fmt.Fprintf(conn, "hello\n")
		Expect(err).NotTo(HaveOccurred())

		line, err := bufio.NewReader(conn).ReadString('\n')
		Expect(err).NotTo(HaveOccurred())
		Expect(line).To(Equal("echo:hello\n"))
	})

	It("ends the client's read when the guest server closes its side", func() {
		guest := startEchoServer()
		defer guest.Close()

		ctx, cancelDial := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelDial()

		conn, err := proxy.DialGuest(ctx, addrPort(guest))
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close()

		_, err = fmt.Fprintf(conn, "bye\n")
		Expect(err).NotTo(HaveOccurred())

		reader := bufio.NewReader(conn)
		_, err = reader.ReadString('\n')
		Expect(err).NotTo(HaveOccurred())

		// The echo server hangs up after one line, which has to reach the host
		// as an ordinary end of stream rather than a connection left open.
		_, err = reader.ReadString('\n')
		Expect(err).To(HaveOccurred())
	})

	It("delivers an answer while the guest keeps the connection open", func() {
		// A browser talking to a keep-alive server is the normal case, and it
		// only works if each direction is flushed as it is written rather than
		// when the connection ends.
		guest, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		defer guest.Close()
		go func() {
			conn, err := guest.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			reader := bufio.NewReader(conn)
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				fmt.Fprintf(conn, "echo:%s\n", strings.TrimSpace(line))
			}
		}()

		ctx, cancelDial := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelDial()

		conn, err := proxy.DialGuest(ctx, addrPort(guest))
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close()

		reader := bufio.NewReader(conn)
		for i := range 3 {
			_, err = fmt.Fprintf(conn, "ping-%d\n", i)
			Expect(err).NotTo(HaveOccurred())

			answered := make(chan string, 1)
			go func() {
				line, err := reader.ReadString('\n')
				if err == nil {
					answered <- line
				}
			}()
			Eventually(answered, "5s").Should(Receive(Equal(fmt.Sprintf("echo:ping-%d\n", i))))
		}
	})

	It("fails the dial when nothing is listening on the port", func() {
		free, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		port := addrPort(free)
		Expect(free.Close()).To(Succeed())

		ctx, cancelDial := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelDial()

		_, err = proxy.DialGuest(ctx, port)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(fmt.Sprintf("dial port %d in the guest", port)))
	})
})

// startEchoServer stands in for a server inside the guest: it answers one line
// and hangs up, listening only on loopback.
func startEchoServer() net.Listener {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil {
					return
				}
				fmt.Fprintf(conn, "echo:%s\n", strings.TrimSpace(line))
			}()
		}
	}()
	return ln
}

func addrPort(ln net.Listener) uint16 {
	return uint16(ln.Addr().(*net.TCPAddr).Port)
}
