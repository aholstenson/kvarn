package link

import (
	"net"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("resolutions", func() {
	It("names an address the forwarder answered with", func() {
		r := newResolutions()
		r.record(net.ParseIP("93.184.216.34"), "db.example.com")

		Expect(r.lookup("93.184.216.34")).To(Equal([]string{"db.example.com"}))
	})

	It("knows nothing about an address it never answered", func() {
		r := newResolutions()
		Expect(r.lookup("10.1.2.3")).To(BeEmpty())
	})

	It("keeps every name an address answers for, most recent first", func() {
		// Shared hosting and CDNs put many names behind one address, and the
		// allowlist only has to find one of them.
		r := newResolutions()
		r.record(net.ParseIP("93.184.216.34"), "one.example.com")
		r.record(net.ParseIP("93.184.216.34"), "two.example.com")

		Expect(r.lookup("93.184.216.34")).To(Equal([]string{"two.example.com", "one.example.com"}))
	})

	It("does not list a repeated name twice", func() {
		r := newResolutions()
		r.record(net.ParseIP("93.184.216.34"), "one.example.com")
		r.record(net.ParseIP("93.184.216.34"), "two.example.com")
		r.record(net.ParseIP("93.184.216.34"), "one.example.com")

		Expect(r.lookup("93.184.216.34")).To(Equal([]string{"one.example.com", "two.example.com"}))
	})

	It("folds case and a trailing dot, which a query may carry either way", func() {
		r := newResolutions()
		r.record(net.ParseIP("93.184.216.34"), "DB.Example.com.")

		Expect(r.lookup("93.184.216.34")).To(Equal([]string{"db.example.com"}))
	})

	It("forgets an address that has gone stale", func() {
		r := newResolutions()
		r.record(net.ParseIP("93.184.216.34"), "db.example.com")
		r.entries["93.184.216.34"].seen = time.Now().Add(-resolutionTTL - time.Minute)

		Expect(r.lookup("93.184.216.34")).To(BeEmpty())
	})

	It("stays inside its cap, dropping the addresses looked up longest ago", func() {
		r := newResolutions()
		base := time.Now()
		addr := func(i int) net.IP { return net.IPv4(10, byte(i>>16), byte(i>>8), byte(i)) }
		total := maxResolutions + 10
		for i := range total {
			ip := addr(i)
			r.record(ip, "host.example.com")
			r.entries[ip.String()].seen = base.Add(time.Duration(i) * time.Millisecond)
		}

		Expect(len(r.entries)).To(BeNumerically("<=", maxResolutions))
		// The most recent address survives; the first one recorded does not.
		Expect(r.lookup(addr(total - 1).String())).NotTo(BeEmpty())
		Expect(r.lookup(addr(0).String())).To(BeEmpty())
	})

	It("caps how many names one address is remembered under", func() {
		r := newResolutions()
		ip := net.ParseIP("93.184.216.34")
		for i := range maxNamesPerAddress + 5 {
			r.record(ip, string(rune('a'+i%26))+"-host.example.com")
		}

		Expect(len(r.lookup("93.184.216.34"))).To(BeNumerically("<=", maxNamesPerAddress))
	})
})
