package sandbox

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	egressproxy "github.com/aholstenson/kvarn/internal/egress/proxy"
	"github.com/aholstenson/kvarn/internal/project"
)

var _ = Describe("egress rules from a network block", func() {
	It("applies the default ports to a bare entry", func() {
		rules := egressRules(project.Network{
			AllowedHosts: []project.AllowedHost{{Host: "api.example.com"}},
		})

		Expect(rules).To(Equal([]egressproxy.Rule{
			{Host: "api.example.com", Ports: []uint16{80, 443}},
		}))
	})

	It("carries ports and the passthrough decision through", func() {
		rules := egressRules(project.Network{
			AllowedHosts: []project.AllowedHost{
				{Host: "db.example.com", Ports: []uint16{5432}},
				{Host: "api.pinned.example.com", TLS: project.TLSPassthrough},
			},
		})

		Expect(rules).To(Equal([]egressproxy.Rule{
			{Host: "db.example.com", Ports: []uint16{5432}},
			{Host: "api.pinned.example.com", Ports: []uint16{80, 443}, Passthrough: true},
		}))
	})

	It("produces nothing for a block that names no hosts", func() {
		Expect(egressRules(project.Network{})).To(BeEmpty())
	})
})

var _ = Describe("host aliases for a preview", func() {
	cfg := func() *project.Config {
		return &project.Config{
			Network: project.Network{
				HostAliases: map[string]string{"dev.example.local": "127.0.0.1"},
			},
			Preview: project.Preview{
				Network: project.Network{
					HostAliases: map[string]string{"tenant.example.local": "127.0.0.2"},
				},
			},
		}
	}

	It("leaves the preview's own aliases out of a job", func() {
		Expect(Opts{Config: cfg()}.hostAliases()).To(Equal(map[string]string{
			"dev.example.local": "127.0.0.1",
		}))
	})

	It("adds the preview's aliases to the project's when serving one", func() {
		Expect(Opts{Config: cfg(), Preview: true}.hostAliases()).To(Equal(map[string]string{
			"dev.example.local":    "127.0.0.1",
			"tenant.example.local": "127.0.0.2",
		}))
	})

	It("lets the caller's aliases win over both", func() {
		// A local preview derives its site hostnames from a base domain given
		// on the command line, which kvarn.yml cannot know.
		opts := Opts{
			Config:      cfg(),
			Preview:     true,
			HostAliases: map[string]string{"tenant.example.local": "127.0.0.9"},
		}
		Expect(opts.hostAliases()).To(HaveKeyWithValue("tenant.example.local", "127.0.0.9"))
	})
})
