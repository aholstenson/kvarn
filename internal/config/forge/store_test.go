package forge_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
)

var _ = Describe("ResolveBehavior", func() {
	It("falls back to compiled-in constants when nothing is set", func() {
		b := (&forgeconfig.ForgeConfig{}).ResolveBehavior(forgeconfig.Defaults{}, forgeconfig.Overrides{})
		Expect(b.BranchPrefix).To(Equal(forgeconfig.DefaultBranchPrefix))
		Expect(b.CommitAuthorName).To(Equal(forgeconfig.DefaultCommitAuthorName))
		Expect(b.CommitAuthorEmail).To(Equal(forgeconfig.DefaultCommitAuthorEmail))
		Expect(b.Labels).To(Equal(forgeconfig.DefaultLabels()))
	})

	It("uses global defaults over the compiled-in constants", func() {
		d := forgeconfig.Defaults{
			BranchPrefix:      "bot",
			CommitAuthorName:  "Global Bot",
			CommitAuthorEmail: "global@example.com",
			Labels:            []string{"global"},
		}
		b := (&forgeconfig.ForgeConfig{}).ResolveBehavior(d, forgeconfig.Overrides{})
		Expect(b.BranchPrefix).To(Equal("bot"))
		Expect(b.CommitAuthorName).To(Equal("Global Bot"))
		Expect(b.CommitAuthorEmail).To(Equal("global@example.com"))
		Expect(b.Labels).To(Equal([]string{"global"}))
	})

	It("lets per-forge values override global defaults", func() {
		d := forgeconfig.Defaults{
			BranchPrefix:      "bot",
			CommitAuthorName:  "Global Bot",
			CommitAuthorEmail: "global@example.com",
			Labels:            []string{"global"},
		}
		fc := &forgeconfig.ForgeConfig{
			BranchPrefix:      "myorg",
			CommitAuthorName:  "Org Bot",
			CommitAuthorEmail: "org@example.com",
			Labels:            []string{"org"},
		}
		b := fc.ResolveBehavior(d, forgeconfig.Overrides{})
		Expect(b.BranchPrefix).To(Equal("myorg"))
		Expect(b.CommitAuthorName).To(Equal("Org Bot"))
		Expect(b.CommitAuthorEmail).To(Equal("org@example.com"))
		Expect(b.Labels).To(Equal([]string{"org"}))
	})

	It("lets per-project overrides win over per-forge values and defaults", func() {
		d := forgeconfig.Defaults{
			BranchPrefix:      "bot",
			CommitAuthorName:  "Global Bot",
			CommitAuthorEmail: "global@example.com",
			Labels:            []string{"global"},
		}
		fc := &forgeconfig.ForgeConfig{
			BranchPrefix:      "myorg",
			CommitAuthorName:  "Org Bot",
			CommitAuthorEmail: "org@example.com",
			Labels:            []string{"org"},
		}
		o := forgeconfig.Overrides{
			BranchPrefix:      "proj",
			CommitAuthorName:  "Project Bot",
			CommitAuthorEmail: "project@example.com",
			Labels:            []string{"project"},
		}
		b := fc.ResolveBehavior(d, o)
		Expect(b.BranchPrefix).To(Equal("proj"))
		Expect(b.CommitAuthorName).To(Equal("Project Bot"))
		Expect(b.CommitAuthorEmail).To(Equal("project@example.com"))
		Expect(b.Labels).To(Equal([]string{"project"}))
	})

	It("layers each field independently", func() {
		// Per-project sets only the labels; the branch prefix comes from the
		// forge, the author name from globals, and the unset author email falls
		// through to the compiled-in constant.
		d := forgeconfig.Defaults{CommitAuthorName: "Global Bot", Labels: []string{"global"}}
		fc := &forgeconfig.ForgeConfig{BranchPrefix: "myorg"}
		o := forgeconfig.Overrides{Labels: []string{"project"}}
		b := fc.ResolveBehavior(d, o)
		Expect(b.BranchPrefix).To(Equal("myorg"))
		Expect(b.CommitAuthorName).To(Equal("Global Bot"))
		Expect(b.CommitAuthorEmail).To(Equal(forgeconfig.DefaultCommitAuthorEmail))
		Expect(b.Labels).To(Equal([]string{"project"}))
	})

	It("replaces labels wholesale rather than merging across layers", func() {
		d := forgeconfig.Defaults{Labels: []string{"global"}}
		fc := &forgeconfig.ForgeConfig{Labels: []string{"forge"}}
		o := forgeconfig.Overrides{Labels: []string{"only-this"}}
		b := fc.ResolveBehavior(d, o)
		Expect(b.Labels).To(Equal([]string{"only-this"}))
	})

	It("handles a nil forge config (project without a forge)", func() {
		var fc *forgeconfig.ForgeConfig
		b := fc.ResolveBehavior(forgeconfig.Defaults{BranchPrefix: "bot"}, forgeconfig.Overrides{})
		Expect(b.BranchPrefix).To(Equal("bot"))
		Expect(b.CommitAuthorName).To(Equal(forgeconfig.DefaultCommitAuthorName))
		Expect(b.Labels).To(Equal(forgeconfig.DefaultLabels()))
	})

	It("applies per-project overrides even with a nil forge config", func() {
		var fc *forgeconfig.ForgeConfig
		o := forgeconfig.Overrides{BranchPrefix: "proj", Labels: []string{"project"}}
		b := fc.ResolveBehavior(forgeconfig.Defaults{BranchPrefix: "bot"}, o)
		Expect(b.BranchPrefix).To(Equal("proj"))
		Expect(b.Labels).To(Equal([]string{"project"}))
	})
})
