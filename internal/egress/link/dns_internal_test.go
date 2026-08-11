package link

import (
	"net"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("dnsForwarder host mappings", func() {
	newForwarder := func(aliases map[string]string) *dnsForwarder {
		return &dnsForwarder{aliases: normalizeAliases(aliases)}
	}

	It("answers an exact name", func() {
		d := newForwarder(map[string]string{"dev-shop.sws.local": "127.0.0.1"})

		ip, ok := d.localAddress("dev-shop.sws.local")
		Expect(ok).To(BeTrue())
		Expect(ip.String()).To(Equal("127.0.0.1"))
	})

	It("answers any subdomain of a wildcard", func() {
		d := newForwarder(map[string]string{"*.sws.local": "127.0.0.1"})

		for _, name := range []string{"dev-shop.sws.local", "a.b.sws.local"} {
			ip, ok := d.localAddress(name)
			Expect(ok).To(BeTrue(), name)
			Expect(ip.String()).To(Equal("127.0.0.1"), name)
		}
	})

	It("does not let a wildcard answer the bare suffix", func() {
		d := newForwarder(map[string]string{"*.sws.local": "127.0.0.1"})

		_, ok := d.localAddress("sws.local")
		Expect(ok).To(BeFalse())
	})

	It("does not match a suffix that is not on a label boundary", func() {
		d := newForwarder(map[string]string{"*.sws.local": "127.0.0.1"})

		_, ok := d.localAddress("evilsws.local")
		Expect(ok).To(BeFalse())
	})

	It("prefers an exact entry over a wildcard covering it", func() {
		d := newForwarder(map[string]string{
			"*.sws.local":        "127.0.0.1",
			"dev-shop.sws.local": "127.0.0.9",
		})

		ip, ok := d.localAddress("dev-shop.sws.local")
		Expect(ok).To(BeTrue())
		Expect(ip.String()).To(Equal("127.0.0.9"))
	})

	It("prefers the longest matching wildcard suffix", func() {
		d := newForwarder(map[string]string{
			"*.sws.local":     "127.0.0.1",
			"*.dev.sws.local": "127.0.0.9",
		})

		ip, ok := d.localAddress("shop.dev.sws.local")
		Expect(ok).To(BeTrue())
		Expect(ip.String()).To(Equal("127.0.0.9"))
	})

	It("matches regardless of the case and trailing dot a query arrives with", func() {
		d := newForwarder(map[string]string{"Dev-Shop.SWS.local": "127.0.0.1"})

		ip, ok := d.localAddress("DEV-shop.sws.LOCAL.")
		Expect(ok).To(BeTrue())
		Expect(ip.String()).To(Equal("127.0.0.1"))
	})

	It("ignores an unmapped name", func() {
		d := newForwarder(map[string]string{"*.sws.local": "127.0.0.1"})

		_, ok := d.localAddress("example.com")
		Expect(ok).To(BeFalse())
	})

	It("resolves nothing when no aliases are configured", func() {
		d := newForwarder(nil)

		_, ok := d.localAddress("dev-shop.sws.local")
		Expect(ok).To(BeFalse())
	})

	Describe("the answers built for a mapped name", func() {
		// Question section for "dev-shop.sws.local", the part buildAnswer
		// copies and points its answer records back at.
		question := func(qtype uint16) []byte {
			msg := []byte{
				0xab, 0xcd, // ID
				0x01, 0x00, // flags: RD
				0x00, 0x01, // QDCOUNT
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			}
			for _, label := range []string{"dev-shop", "sws", "local"} {
				msg = append(msg, byte(len(label)))
				msg = append(msg, label...)
			}
			return append(msg, 0x00, byte(qtype>>8), byte(qtype), 0x00, 0x01)
		}

		It("carries one record for the matching family", func() {
			resp := buildAnswer(question(qtypeA), "dev-shop.sws.local", qtypeA,
				[]net.IP{net.ParseIP("127.0.0.1")})

			Expect(resp[6:8]).To(Equal([]byte{0x00, 0x01}))
			Expect(resp[len(resp)-4:]).To(Equal([]byte{127, 0, 0, 1}))
		})

		It("carries no records for the other family, leaving the name known", func() {
			resp := buildAnswer(question(qtypeAAAA), "dev-shop.sws.local", qtypeAAAA,
				[]net.IP{net.ParseIP("127.0.0.1")})

			// NOERROR with zero answers, not NXDOMAIN: the name exists, it
			// just has no AAAA. A resolver told NXDOMAIN would stop asking.
			Expect(resp[6:8]).To(Equal([]byte{0x00, 0x00}))
			Expect(resp[3] & 0x0f).To(Equal(byte(0)))
		})
	})
})
