package sandbox

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox/cache"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// stubProxy implements only the RunnerProxy methods the tools code calls; the
// embedded interface is nil, so anything else panics rather than passing
// silently.
type stubProxy struct {
	RunnerProxy
	uploaded  *v1.UploadFilesRequest
	commands  []string
	response  *v1.SessionExecResponse
	execError error
}

func (p *stubProxy) UploadFiles(_ context.Context, req *v1.UploadFilesRequest) (*v1.UploadFilesResponse, error) {
	p.uploaded = req
	return &v1.UploadFilesResponse{}, nil
}

func (p *stubProxy) SessionExec(_ context.Context, req *v1.SessionExecRequest, onOutput OutputCallback) (*v1.SessionExecResponse, error) {
	p.commands = append(p.commands, req.Command)
	if p.execError != nil {
		return nil, p.execError
	}
	if onOutput != nil {
		onOutput("installing\n", "")
	}
	if p.response != nil {
		return p.response, nil
	}
	return &v1.SessionExecResponse{ExitCode: 0}, nil
}

// uploadedFile returns the content of one file from the recorded upload.
func (p *stubProxy) uploadedFile(name string) (string, bool) {
	if p.uploaded == nil {
		return "", false
	}
	for _, f := range p.uploaded.Files {
		if f.Path == name {
			return string(f.Content), true
		}
	}
	return "", false
}

func (p *stubProxy) uploadedNames() []string {
	var names []string
	if p.uploaded == nil {
		return names
	}
	for _, f := range p.uploaded.Files {
		names = append(names, f.Path)
	}
	return names
}

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
		Expect(aug.Hosts).To(BeEmpty())
		Expect(aug.Env).To(BeEmpty())
		Expect(aug.PathPrepend).To(BeEmpty())
	})

	It("populates from a single nixpkgs dep", func() {
		deps := []project.ResolvedDep{
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "go", Host: "github.com"},
		}
		aug := computeAugmentations(deps)
		Expect(aug.Hosts).To(ContainElements("proxy.golang.org", "sum.golang.org"))
		Expect(aug.Env).To(HaveKeyWithValue("GOPATH", "/home/kvarn/go"))
		Expect(aug.PathPrepend).To(ConsistOf("/home/kvarn/go/bin"))
	})

	It("dedups overlapping hosts and PATH entries across deps", func() {
		// `cargo` and `rustc` both contribute /home/kvarn/.cargo + crates.io.
		deps := []project.ResolvedDep{
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "cargo", Host: "github.com"},
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "rustc", Host: "github.com"},
		}
		aug := computeAugmentations(deps)
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
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "go", Host: "github.com"},
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "cargo", Host: "github.com"},
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
		Expect(aug.Hosts).To(BeEmpty())
		Expect(aug.Env).To(BeEmpty())
		Expect(aug.PathPrepend).To(BeEmpty())
		Expect(aug.Provision).To(BeEmpty())
	})

	It("matches nixpkgs deps even on alternate channels", func() {
		deps := []project.ResolvedDep{
			{FlakeURI: "github:NixOS/nixpkgs/nixos-unstable", Attr: "bun", Host: "github.com"},
		}
		aug := computeAugmentations(deps)
		Expect(aug.Hosts).To(ContainElement("bun.sh"))
	})

	It("puts a version manager's shims ahead of the tools it shadows", func() {
		// The profile script prepends line by line, so the last PATH entry is
		// the one that ends up first. A repository declaring both means its
		// mise.toml pin to win over the Nix-provided toolchain.
		deps := []project.ResolvedDep{
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "mise", Host: "github.com"},
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "go", Host: "github.com"},
		}
		aug := computeAugmentations(deps)
		Expect(aug.PathPrepend).To(Equal([]string{
			"/home/kvarn/go/bin",
			"/home/kvarn/.local/share/mise/shims",
		}))
	})

	It("orders PATH and env the same way regardless of dependency order", func() {
		a := computeAugmentations([]project.ResolvedDep{
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "cargo"},
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "mise"},
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "go"},
		})
		b := computeAugmentations([]project.ResolvedDep{
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "go"},
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "cargo"},
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "mise"},
		})
		Expect(a.PathPrepend).To(Equal(b.PathPrepend))
		Expect(a.Hosts).To(Equal(b.Hosts))
		Expect(a.Env).To(Equal(b.Env))
	})

	It("collects provisioning commands tagged with the tool that asked", func() {
		deps := []project.ResolvedDep{
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "mise", Host: "github.com"},
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "go", Host: "github.com"},
		}
		aug := computeAugmentations(deps)
		Expect(aug.Provision).To(Equal([]provisionStep{
			{Tool: "mise", Command: "mise install"},
		}))
	})

	It("collects no provisioning commands for tools that need none", func() {
		aug := computeAugmentations([]project.ResolvedDep{
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "go"},
		})
		Expect(aug.Provision).To(BeEmpty())
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

var _ = Describe("writeProfileScripts", func() {
	var proxy *stubProxy

	BeforeEach(func() { proxy = &stubProxy{} })

	It("writes PATH to a file that sorts after the Nix profile's", func() {
		// /etc/profile sources the directory in sorted order and the image
		// links the Nix profile in as nix.sh. A `kvarn-` name would sort ahead
		// of it and have its PATH entries pushed behind ~/.nix-profile/bin.
		aug := augmentations{
			Env:         map[string]string{"GOPATH": "/home/kvarn/go"},
			PathPrepend: []string{"/home/kvarn/.local/share/mise/shims"},
		}
		Expect(writeProfileScripts(context.Background(), proxy, aug, nil, nil)).To(Succeed())

		names := proxy.uploadedNames()
		Expect(names).To(ContainElement("zz-kvarn-path.sh"))
		sorted := append([]string(nil), names...)
		sort.Strings(sorted)
		Expect(sorted[len(sorted)-1]).To(Equal("zz-kvarn-path.sh"))
		Expect("zz-kvarn-path.sh" > "nix.sh").To(BeTrue())

		path, ok := proxy.uploadedFile("zz-kvarn-path.sh")
		Expect(ok).To(BeTrue())
		Expect(path).To(Equal(`export PATH='/home/kvarn/.local/share/mise/shims':"$PATH"` + "\n"))
	})

	It("keeps tool, user and secret environment in their sourcing order", func() {
		aug := augmentations{Env: map[string]string{"GOPATH": "/home/kvarn/go"}}
		Expect(writeProfileScripts(context.Background(), proxy, aug,
			map[string]string{"CI": "true"}, map[string]string{"TOKEN": "t"})).To(Succeed())

		Expect(proxy.uploadedNames()).To(Equal([]string{
			"kvarn-tools.sh", "kvarn-user.sh", "kvarn-secrets.sh",
		}))
		tools, _ := proxy.uploadedFile("kvarn-tools.sh")
		Expect(tools).NotTo(ContainSubstring("export PATH="))
	})

	It("writes nothing when there is nothing to write", func() {
		Expect(writeProfileScripts(context.Background(), proxy, augmentations{}, nil, nil)).To(Succeed())
		Expect(proxy.uploaded).To(BeNil())
	})
})

var _ = Describe("provisionTool", func() {
	step := provisionStep{Tool: "mise", Command: "mise install"}

	It("runs the command in the job's shell session and streams output", func() {
		proxy := &stubProxy{}
		var streamed string
		Expect(provisionTool(context.Background(), proxy, "session-1", step,
			func(stdout, stderr string) { streamed += stdout })).To(Succeed())

		Expect(proxy.commands).To(Equal([]string{"mise install"}))
		Expect(streamed).To(Equal("installing\n"))
	})

	It("names the tool and the command when the command fails", func() {
		proxy := &stubProxy{response: &v1.SessionExecResponse{ExitCode: 3, Stderr: "no such tool\n"}}
		err := provisionTool(context.Background(), proxy, "session-1", step, nil)
		Expect(err).To(MatchError(ContainSubstring("provision mise")))
		Expect(err).To(MatchError(ContainSubstring("mise install")))
		Expect(err).To(MatchError(ContainSubstring("no such tool")))
	})

	It("propagates a transport failure", func() {
		proxy := &stubProxy{execError: errors.New("bridge closed")}
		Expect(provisionTool(context.Background(), proxy, "session-1", step, nil)).
			To(MatchError(ContainSubstring("bridge closed")))
	})
})

var _ = Describe("overlapping cache claims", func() {
	It("gives a shared path to the more specific tool", func() {
		// `openjdk` claims ~/.gradle so a project that declares only a JDK and
		// builds through gradlew still caches; `gradle` claims it with a key
		// derived from the build files. Declaring both must resolve to the
		// sharper key, not to whichever the dependency map yielded first.
		deps := []project.ResolvedDep{
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "openjdk"},
			{FlakeURI: project.DefaultNixpkgsFlake, Attr: "gradle"},
		}
		layers, err := cache.DeriveLayers(GinkgoT().TempDir(), deps, cacheToolLookup,
			project.Cache{}, "project-1", "")
		Expect(err).NotTo(HaveOccurred())

		var gradleBucket string
		for _, l := range layers {
			if l.GuestPath == "/home/kvarn/.gradle" {
				gradleBucket = l.Key.Bucket
			}
		}
		Expect(gradleBucket).To(Equal("gradle"))
	})
})

var _ = Describe("the registered tool documentation", func() {
	It("has a row for every registry entry", func() {
		// Adding an entry is how a tool becomes supported, so the reference
		// page is part of the change rather than a follow-up someone forgets.
		doc, err := os.ReadFile("../../docs/reference/registered-tools.md")
		Expect(err).NotTo(HaveOccurred())
		for name := range toolRegistry {
			Expect(string(doc)).To(ContainSubstring("`"+name+"`"),
				"registry entry %q is missing from docs/reference/registered-tools.md", name)
		}
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
