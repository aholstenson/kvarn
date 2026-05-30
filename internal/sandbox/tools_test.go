package sandbox

import (
	"strings"

	"github.com/aholstenson/kvarn/internal/project"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("lookupCurated", func() {
	It("matches an exact attr", func() {
		e, ok := lookupTool("go")
		Expect(ok).To(BeTrue())
		Expect(e.CachePaths).To(ContainElement("/home/kvarn/go"))
	})

	It("matches a versioned _NN suffix when entry opts in", func() {
		e, ok := lookupTool("go_1_22")
		Expect(ok).To(BeTrue())
		Expect(e.CachePaths).To(ContainElement("/home/kvarn/go"))
	})

	It("matches a trailing-digits attr like python312", func() {
		e, ok := lookupTool("python312")
		Expect(ok).To(BeTrue())
		Expect(e.Hosts).To(ContainElement("pypi.org"))
	})

	It("returns not-found for unknown attrs", func() {
		_, ok := lookupTool("definitely-not-a-real-pkg")
		Expect(ok).To(BeFalse())
	})

	It("does not strip suffixes for entries that opt out", func() {
		// `coreutils` is not registered. With strip enabled, `coreutils`
		// would normalize to `coreutil` (trailing digits -> nothing) or
		// stay; either way, no curated match must come back.
		_, ok := lookupTool("coreutils")
		Expect(ok).To(BeFalse())
	})
})

var _ = Describe("computeAugmentations", func() {
	It("returns empty for no deps", func() {
		aug := computeAugmentations(nil)
		Expect(aug.CachePaths).To(BeEmpty())
		Expect(aug.Hosts).To(BeEmpty())
		Expect(aug.Env).To(BeEmpty())
		Expect(aug.PathPrepend).To(BeEmpty())
	})

	It("populates from a single nixpkgs dep", func() {
		deps := []project.ResolvedDep{
			{FlakeURI: "github:NixOS/nixpkgs/nixos-25.11", Attr: "go", Host: "github.com"},
		}
		aug := computeAugmentations(deps)
		Expect(aug.CachePaths).To(ConsistOf("/home/kvarn/go"))
		Expect(aug.Hosts).To(ContainElements("proxy.golang.org", "sum.golang.org"))
		Expect(aug.Env).To(HaveKeyWithValue("GOPATH", "/home/kvarn/go"))
		Expect(aug.PathPrepend).To(ConsistOf("/home/kvarn/go/bin"))
	})

	It("dedups overlapping cache paths and hosts across deps", func() {
		// `cargo` and `rustc` both contribute /home/kvarn/.cargo + crates.io.
		deps := []project.ResolvedDep{
			{FlakeURI: "github:NixOS/nixpkgs/nixos-25.11", Attr: "cargo", Host: "github.com"},
			{FlakeURI: "github:NixOS/nixpkgs/nixos-25.11", Attr: "rustc", Host: "github.com"},
		}
		aug := computeAugmentations(deps)
		Expect(aug.CachePaths).To(ConsistOf("/home/kvarn/.cargo"))
		// Hosts deduped.
		hostCount := 0
		for _, h := range aug.Hosts {
			if h == "crates.io" {
				hostCount++
			}
		}
		Expect(hostCount).To(Equal(1))
		Expect(aug.PathPrepend).To(ConsistOf("/home/kvarn/.cargo/bin"))
	})

	It("merges env across deps", func() {
		deps := []project.ResolvedDep{
			{FlakeURI: "github:NixOS/nixpkgs/nixos-25.11", Attr: "go", Host: "github.com"},
			{FlakeURI: "github:NixOS/nixpkgs/nixos-25.11", Attr: "cargo", Host: "github.com"},
		}
		aug := computeAugmentations(deps)
		Expect(aug.Env).To(HaveKeyWithValue("GOPATH", "/home/kvarn/go"))
		Expect(aug.Env).To(HaveKeyWithValue("CARGO_HOME", "/home/kvarn/.cargo"))
	})

	It("skips non-nixpkgs flake URIs", func() {
		deps := []project.ResolvedDep{
			{FlakeURI: "github:owner/custom-flake", Attr: "go", Host: "github.com"},
			{FlakeURI: "git+https://example.com/flake", Attr: "cargo", Host: "example.com"},
		}
		aug := computeAugmentations(deps)
		Expect(aug.CachePaths).To(BeEmpty())
		Expect(aug.Hosts).To(BeEmpty())
		Expect(aug.Env).To(BeEmpty())
		Expect(aug.PathPrepend).To(BeEmpty())
	})

	It("matches nixpkgs deps even on alternate channels", func() {
		deps := []project.ResolvedDep{
			{FlakeURI: "github:NixOS/nixpkgs/nixos-unstable", Attr: "bun", Host: "github.com"},
		}
		aug := computeAugmentations(deps)
		Expect(aug.CachePaths).To(ContainElement("/home/kvarn/.bun/install/cache"))
	})
})

var _ = Describe("buildProfileScript", func() {
	It("returns empty when both inputs are empty", func() {
		Expect(buildProfileScript(nil, nil)).To(Equal(""))
	})

	It("renders a single env var", func() {
		s := buildProfileScript(map[string]string{"FOO": "bar"}, nil)
		Expect(s).To(Equal("export FOO='bar'\n"))
	})

	It("orders env keys deterministically", func() {
		s := buildProfileScript(map[string]string{"BBB": "2", "AAA": "1"}, nil)
		Expect(s).To(Equal("export AAA='1'\nexport BBB='2'\n"))
	})

	It("escapes embedded single quotes", func() {
		s := buildProfileScript(map[string]string{"Q": "it's"}, nil)
		Expect(s).To(Equal(`export Q='it'\''s'` + "\n"))
	})

	It("renders PATH prepends in order, left-to-right", func() {
		s := buildProfileScript(nil, []string{"/a", "/b"})
		// The shell sources lines in order, so the LAST prepend wins
		// at the front of PATH. Test that both lines are emitted in
		// insertion order.
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		Expect(lines).To(HaveLen(2))
		Expect(lines[0]).To(Equal(`export PATH='/a':"$PATH"`))
		Expect(lines[1]).To(Equal(`export PATH='/b':"$PATH"`))
	})

	It("combines env and PATH prepends", func() {
		s := buildProfileScript(map[string]string{"X": "y"}, []string{"/bin"})
		Expect(s).To(ContainSubstring(`export X='y'`))
		Expect(s).To(ContainSubstring(`export PATH='/bin':"$PATH"`))
	})
})

var _ = Describe("curated cache paths", func() {
	It("does not place any path under /home/kvarn/workspace or /nix/", func() {
		var all []string
		for _, e := range toolRegistry {
			all = append(all, e.CachePaths...)
		}
		for _, p := range all {
			Expect(strings.HasPrefix(p, "/home/kvarn/workspace")).To(BeFalse(), "path %q under workspace", p)
			Expect(p).NotTo(Equal("/nix"))
			Expect(p).NotTo(Equal("/nix/store"))
			Expect(strings.HasPrefix(p, "/nix/")).To(BeFalse(), "path %q under /nix/", p)
		}
	})
})
