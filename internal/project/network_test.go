package project_test

import (
	"github.com/aholstenson/kvarn/internal/project"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Network allowed_hosts entries", func() {
	var dir string

	BeforeEach(func() { dir = GinkgoT().TempDir() })

	load := func(body string) (*project.Config, error) {
		writeYAML(dir, "kvarn.yml", body)
		return project.Load(dir)
	}

	It("reads a bare entry as the host on ports 80 and 443", func() {
		cfg, err := load(`
network:
  allowed_hosts:
    - api.example.com
`)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Network.AllowedHosts).To(HaveLen(1))

		entry := cfg.Network.AllowedHosts[0]
		Expect(entry.Host).To(Equal("api.example.com"))
		Expect(entry.EffectivePorts()).To(Equal([]uint16{80, 443}))
		Expect(entry.Passthrough()).To(BeFalse())
	})

	It("reads an entry that names its own ports", func() {
		cfg, err := load(`
network:
  allowed_hosts:
    - host: db.staging.example.com
      ports: [5432]
`)
		Expect(err).NotTo(HaveOccurred())

		entry := cfg.Network.AllowedHosts[0]
		Expect(entry.Host).To(Equal("db.staging.example.com"))
		Expect(entry.EffectivePorts()).To(Equal([]uint16{5432}))
	})

	It("reads an entry asking for TLS passthrough", func() {
		cfg, err := load(`
network:
  allowed_hosts:
    - host: api.pinned.example.com
      tls: passthrough
`)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Network.AllowedHosts[0].Passthrough()).To(BeTrue())
	})

	It("mixes bare and mapping entries in one list", func() {
		cfg, err := load(`
network:
  allowed_hosts:
    - "*.stripe.com"
    - host: db.staging.example.com
      ports: [5432]
`)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Network.AllowedHosts).To(Equal([]project.AllowedHost{
			{Host: "*.stripe.com"},
			{Host: "db.staging.example.com", Ports: []uint16{5432}},
		}))
	})

	DescribeTable("rejects an entry that cannot work",
		func(body, wantErr string) {
			_, err := load(body)
			Expect(err).To(MatchError(ContainSubstring(wantErr)))
		},
		Entry("port 0, which names nothing",
			`
network:
  allowed_hosts:
    - host: db.example.com
      ports: [0]
`, "lists port 0"),
		Entry("an unknown TLS mode",
			`
network:
  allowed_hosts:
    - host: api.example.com
      tls: sniff
`, "want \"inspect\" or \"passthrough\""),
		Entry("a TLS mode on an entry that never opens 443",
			// The mode governs port 443 alone, so this would be silently
			// ignored — and a security setting that does nothing is worse
			// than one that is rejected.
			`
network:
  allowed_hosts:
    - host: db.example.com
      ports: [5432]
      tls: passthrough
`, "does not list port 443"),
		Entry("a port written into the host string",
			`
network:
  allowed_hosts:
    - "db.example.com:5432"
`, "must not contain a port"),
	)
})

var _ = Describe("preview.network", func() {
	var dir string

	BeforeEach(func() { dir = GinkgoT().TempDir() })

	load := func(body string) (*project.Config, error) {
		writeYAML(dir, "kvarn.yml", body)
		return project.Load(dir)
	}

	It("is read alongside the top-level block and kept apart from it", func() {
		cfg, err := load(`
network:
  allowed_hosts:
    - api.example.com
preview:
  sites:
    web: { port: 3000 }
  network:
    allowed_hosts:
      - "*.stripe.com"
      - host: db.staging.example.com
        ports: [5432]
`)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Network.AllowedHosts).To(Equal([]project.AllowedHost{
			{Host: "api.example.com"},
		}))
		Expect(cfg.Preview.Network.AllowedHosts).To(Equal([]project.AllowedHost{
			{Host: "*.stripe.com"},
			{Host: "db.staging.example.com", Ports: []uint16{5432}},
		}))
	})

	It("carries host aliases of its own", func() {
		cfg, err := load(`
preview:
  sites:
    web: { port: 3000 }
  network:
    host_aliases:
      tenant.dev.example.local: 127.0.0.1
`)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Preview.Network.HostAliases).To(HaveKeyWithValue("tenant.dev.example.local", "127.0.0.1"))
	})

	It("applies the same entry rules as the top-level block", func() {
		_, err := load(`
preview:
  sites:
    web: { port: 3000 }
  network:
    allowed_hosts:
      - "https://api.example.com"
`)
		Expect(err).To(MatchError(ContainSubstring("must not contain a scheme")))
	})

	It("rejects a network block on a preview with no sites", func() {
		_, err := load(`
preview:
  network:
    allowed_hosts:
      - api.example.com
`)
		Expect(err).To(MatchError(ContainSubstring("but no sites")))
	})
})
