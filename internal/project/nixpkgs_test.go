package project_test

import (
	"os"

	"github.com/aholstenson/kvarn/internal/project"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Moving to a newer nixpkgs release is an edit to DefaultNixpkgsChannel and
// DefaultNixpkgsRev. These specs are what keeps the two honest: the channel is
// a promise made to users in the reference docs, and the rev is what jobs
// actually install, so neither may drift behind the other.
var _ = Describe("the default nixpkgs channel", func() {
	It("names a stable NixOS release", func() {
		// Stable releases are what a repository gets when it does not pin;
		// `nixos-unstable` as the default would move under jobs mid-week.
		Expect(project.DefaultNixpkgsChannel).To(MatchRegexp(`^nixos-\d{2}\.\d{2}$`))
	})

	It("pins an exact commit", func() {
		// A branch name here would put a call to api.github.com in front of
		// every cold dependency install, which is the outage the rev avoids.
		Expect(project.DefaultNixpkgsRev).To(MatchRegexp(`^[0-9a-f]{40}$`))
	})

	It("resolves to the flake URI a bare `nixpkgs` source expands to", func() {
		Expect(project.DefaultNixpkgsFlake).To(Equal(
			project.NixpkgsFlakePrefix + project.DefaultNixpkgsRev))
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
