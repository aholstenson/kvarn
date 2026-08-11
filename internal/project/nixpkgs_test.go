package project_test

import (
	"os"

	"github.com/aholstenson/kvarn/internal/project"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Moving to a newer nixpkgs release is a one-line edit to
// DefaultNixpkgsChannel. These specs are what keeps it that way: the channel is
// a promise made to users in the reference docs, so it has to move in the same
// change rather than drift behind the code.
var _ = Describe("the default nixpkgs channel", func() {
	It("names a stable NixOS release", func() {
		// Stable releases are what a repository gets when it does not pin;
		// `nixos-unstable` as the default would move under jobs mid-week.
		Expect(project.DefaultNixpkgsChannel).To(MatchRegexp(`^nixos-\d{2}\.\d{2}$`))
	})

	It("resolves to the flake URI a bare `nixpkgs` source expands to", func() {
		Expect(project.DefaultNixpkgsFlake).To(Equal(
			project.NixpkgsFlakePrefix + project.DefaultNixpkgsChannel))
	})

	It("is the channel the kvarn.yml reference documents", func() {
		doc, err := os.ReadFile("../../docs/reference/kvarn-yml.md")
		Expect(err).NotTo(HaveOccurred())
		Expect(string(doc)).To(ContainSubstring("`"+project.DefaultNixpkgsChannel+"`"),
			"docs/reference/kvarn-yml.md does not mention %s: update the `nixpkgs` row in "+
				"the dependency source table to the current default channel",
			project.DefaultNixpkgsChannel)
	})
})
