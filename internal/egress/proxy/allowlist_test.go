package proxy_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/egress/proxy"
)

var _ = Describe("Allowlist rules", func() {
	It("opens HTTP and HTTPS for an entry that names no ports", func() {
		a := proxy.NewAllowlist([]proxy.Rule{{Host: "api.example.com"}})

		Expect(a.Permit("api.example.com", 80)).To(BeTrue())
		Expect(a.Permit("api.example.com", 443)).To(BeTrue())
		Expect(a.Permit("api.example.com", 5432)).To(BeFalse())
	})

	It("opens only the ports an entry names", func() {
		a := proxy.NewAllowlist([]proxy.Rule{{Host: "db.example.com", Ports: []uint16{5432}}})

		Expect(a.Permit("db.example.com", 5432)).To(BeTrue())
		Expect(a.Permit("db.example.com", 443)).To(BeFalse())
	})

	It("unions the ports of every entry covering a host", func() {
		// A wildcard and the exact host under it are two ways to say the same
		// name, and neither is meant to take away what the other granted.
		a := proxy.NewAllowlist([]proxy.Rule{
			{Host: "*.example.com"},
			{Host: "db.example.com", Ports: []uint16{5432}},
		})

		Expect(a.Permit("db.example.com", 443)).To(BeTrue())
		Expect(a.Permit("db.example.com", 5432)).To(BeTrue())
		Expect(a.Permit("other.example.com", 5432)).To(BeFalse())
	})

	It("reports the passthrough entry when two cover the same port", func() {
		// A host is named for passthrough because inspecting it breaks, and
		// that stays true however else the file happens to list it.
		a := proxy.NewAllowlist([]proxy.Rule{
			{Host: "*.example.com"},
			{Host: "api.example.com", Passthrough: true},
		})

		rule, ok := a.Lookup("api.example.com", 443)
		Expect(ok).To(BeTrue())
		Expect(rule.Passthrough).To(BeTrue())
	})

	It("answers PermitAny for a host allowed on any port at all", func() {
		a := proxy.NewAllowlist([]proxy.Rule{{Host: "db.example.com", Ports: []uint16{5432}}})

		Expect(a.PermitAny("db.example.com")).To(BeTrue())
		Expect(a.PermitAny("other.example.com")).To(BeFalse())
	})

	It("lists the ports beyond HTTP that need their own listener", func() {
		a := proxy.NewAllowlist([]proxy.Rule{
			{Host: "api.example.com"},
			{Host: "db.example.com", Ports: []uint16{5432, 443}},
			{Host: "cache.example.com", Ports: []uint16{6379}},
			{Host: "*.mail.example.com", Ports: []uint16{587, 5432}},
		})

		Expect(a.ExtraPorts()).To(Equal([]uint16{587, 5432, 6379}))
	})

	It("needs no extra listener when nothing asked for one", func() {
		a := proxy.NewHostAllowlist([]string{"api.example.com"})

		Expect(a.ExtraPorts()).To(BeEmpty())
	})

	It("adds a host at runtime on the default ports", func() {
		a := proxy.NewAllowlist(nil)
		a.Add("api.example.com")

		Expect(a.Permit("api.example.com", 443)).To(BeTrue())
		Expect(a.Permit("api.example.com", 5432)).To(BeFalse())
	})
})
