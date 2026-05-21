package link_test

import (
	"context"
	"net"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"

	"github.com/aholstenson/kvarn/internal/egress/link"
)

var _ = Describe("Network", func() {
	It("constructs a stack and binds a TCP listener on the gateway IP", func() {
		ep := channel.New(16, 1500, tcpip.LinkAddress("\x02\x00\x00\x00\x00\x01"))
		defer ep.Close()

		n, err := link.New(link.Config{Endpoint: ep})
		Expect(err).NotTo(HaveOccurred())
		defer n.Close()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { _ = n.Run(ctx) }()

		ln, err := n.Listen(443)
		Expect(err).NotTo(HaveOccurred())
		Expect(ln).NotTo(BeNil())

		addr, ok := ln.Addr().(*net.TCPAddr)
		Expect(ok).To(BeTrue())
		Expect(addr.Port).To(Equal(443))
	})
})
