package tomlstore_test

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
	"github.com/aholstenson/kvarn/internal/config/forge/tomlstore"
	generic "github.com/aholstenson/kvarn/internal/config/tomlstore"
)

var _ = Describe("Forge Config TomlStore", func() {
	var (
		store  *tomlstore.Store
		tmpDir string
		ctx    context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		tmpDir, err = os.MkdirTemp("", "forge-store-test-*")
		Expect(err).NotTo(HaveOccurred())
		store = tomlstore.New(filepath.Join(tmpDir, "forges.toml"))
	})

	AfterEach(func() {
		os.RemoveAll(tmpDir)
	})

	It("puts and gets a forge config", func() {
		err := store.Put(ctx, &forgeconfig.ForgeConfig{
			Name:       "github-myorg",
			Type:       "github",
			Credential: "myorg-pat",
		})
		Expect(err).NotTo(HaveOccurred())

		fc, err := store.Get(ctx, "github-myorg")
		Expect(err).NotTo(HaveOccurred())
		Expect(fc.Name).To(Equal("github-myorg"))
		Expect(fc.Type).To(Equal("github"))
		Expect(fc.Credential).To(Equal("myorg-pat"))
	})

	It("lists forge configs", func() {
		Expect(store.Put(ctx, &forgeconfig.ForgeConfig{Name: "a", Type: "github"})).To(Succeed())
		Expect(store.Put(ctx, &forgeconfig.ForgeConfig{Name: "b", Type: "git"})).To(Succeed())

		configs, err := store.List(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(configs).To(HaveLen(2))
	})

	It("deletes a forge config", func() {
		Expect(store.Put(ctx, &forgeconfig.ForgeConfig{Name: "deleteme", Type: "git"})).To(Succeed())

		err := store.Delete(ctx, "deleteme")
		Expect(err).NotTo(HaveOccurred())

		_, err = store.Get(ctx, "deleteme")
		Expect(err).To(HaveOccurred())
	})

	It("returns ErrNotFound for missing forge config", func() {
		_, err := store.Get(ctx, "nonexistent")
		Expect(err).To(MatchError(generic.ErrNotFound))
	})

	It("returns ErrNotFound when deleting missing forge config", func() {
		err := store.Delete(ctx, "nonexistent")
		Expect(err).To(MatchError(generic.ErrNotFound))
	})

	It("stores all fields", func() {
		Expect(store.Put(ctx, &forgeconfig.ForgeConfig{
			Name:              "full",
			Type:              "github",
			Credential:        "my-cred",
			BranchPrefix:      "automated",
			Labels:            []string{"bot", "auto"},
			CommitAuthorName:  "MyBot",
			CommitAuthorEmail: "bot@myorg.com",
		})).To(Succeed())

		fc, err := store.Get(ctx, "full")
		Expect(err).NotTo(HaveOccurred())
		Expect(fc.Type).To(Equal("github"))
		Expect(fc.Credential).To(Equal("my-cred"))
		Expect(fc.BranchPrefix).To(Equal("automated"))
		Expect(fc.Labels).To(Equal([]string{"bot", "auto"}))
		Expect(fc.CommitAuthorName).To(Equal("MyBot"))
		Expect(fc.CommitAuthorEmail).To(Equal("bot@myorg.com"))
	})

	It("updates an existing forge config", func() {
		Expect(store.Put(ctx, &forgeconfig.ForgeConfig{Name: "fc", Type: "git"})).To(Succeed())
		Expect(store.Put(ctx, &forgeconfig.ForgeConfig{Name: "fc", Type: "github"})).To(Succeed())

		fc, err := store.Get(ctx, "fc")
		Expect(err).NotTo(HaveOccurred())
		Expect(fc.Type).To(Equal("github"))
	})

	It("handles missing file gracefully", func() {
		configs, err := store.List(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(configs).To(BeEmpty())
	})

	It("returns zero-value defaults when no [defaults] block is present", func() {
		Expect(store.Put(ctx, &forgeconfig.ForgeConfig{Name: "a", Type: "github"})).To(Succeed())

		d, err := store.Defaults(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(d).To(Equal(forgeconfig.Defaults{}))
	})

	It("returns zero-value defaults for a missing file", func() {
		d, err := store.Defaults(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(d).To(Equal(forgeconfig.Defaults{}))
	})

	It("parses the [defaults] block", func() {
		path := filepath.Join(tmpDir, "forges.toml")
		content := `[defaults]
branch_prefix       = "bot"
commit_author_name  = "Global Bot"
commit_author_email = "global@example.com"
labels              = ["automated", "kvarn"]

[forges.github-myorg]
type          = "github"
branch_prefix = "myorg"
`
		Expect(os.WriteFile(path, []byte(content), 0644)).To(Succeed())

		d, err := store.Defaults(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.BranchPrefix).To(Equal("bot"))
		Expect(d.CommitAuthorName).To(Equal("Global Bot"))
		Expect(d.CommitAuthorEmail).To(Equal("global@example.com"))
		Expect(d.Labels).To(Equal([]string{"automated", "kvarn"}))

		// The named forge override is still readable alongside the defaults.
		fc, err := store.Get(ctx, "github-myorg")
		Expect(err).NotTo(HaveOccurred())
		Expect(fc.BranchPrefix).To(Equal("myorg"))
	})

	It("surfaces a parse error from Defaults rather than silently falling back", func() {
		path := filepath.Join(tmpDir, "forges.toml")
		Expect(os.WriteFile(path, []byte("not = valid = toml"), 0o644)).To(Succeed())

		_, err := store.Defaults(ctx)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("parse "))
	})

	It("preserves an existing [defaults] block across a Put", func() {
		path := filepath.Join(tmpDir, "forges.toml")
		content := `[defaults]
branch_prefix = "bot"
`
		Expect(os.WriteFile(path, []byte(content), 0644)).To(Succeed())

		Expect(store.Put(ctx, &forgeconfig.ForgeConfig{Name: "new", Type: "git"})).To(Succeed())

		d, err := store.Defaults(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.BranchPrefix).To(Equal("bot"))
	})

	Describe("pull_request blocks", func() {
		It("reads the block from [defaults] and from a named forge", func() {
			path := filepath.Join(tmpDir, "forges.toml")
			content := `[defaults.pull_request]
title_instructions = "Use Conventional Commits."
title_max_length = 60
body_footer = "kvarn · {{ .SessionID }}"
commit_trailers = ["Kvarn-Session: {{ .SessionID }}"]
report_worklog_on_pr = false

[defaults.pull_request.comment_headers]
new_pull_request = "**Issue:** {{ .Metadata.issue_id }}"
pr_comment = "Review for {{ .Metadata.issue_id }}"

[forges.github-myorg]
type = "github"

[forges.github-myorg.pull_request]
body_instructions = "Link the tracking issue."
report_cost_on_pr = true
`
			Expect(os.WriteFile(path, []byte(content), 0644)).To(Succeed())

			d, err := store.Defaults(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.PullRequest.TitleInstructions).To(Equal("Use Conventional Commits."))
			Expect(*d.PullRequest.TitleMaxLength).To(Equal(60))
			Expect(d.PullRequest.BodyFooter).To(Equal("kvarn · {{ .SessionID }}"))
			Expect(d.PullRequest.CommentHeaders.NewPullRequest).To(Equal("**Issue:** {{ .Metadata.issue_id }}"))
			Expect(d.PullRequest.CommentHeaders.PRComment).To(Equal("Review for {{ .Metadata.issue_id }}"))
			Expect(d.PullRequest.CommentHeaders.FollowUpCommit).To(BeEmpty(),
				"a kind the block did not set must stay unset so it can inherit")
			Expect(d.PullRequest.CommitTrailers).To(Equal([]string{"Kvarn-Session: {{ .SessionID }}"}))
			Expect(*d.PullRequest.ReportWorklog).To(BeFalse())
			Expect(d.PullRequest.ReportCost).To(BeNil(), "an unset toggle must stay unset so it can inherit")

			fc, err := store.Get(ctx, "github-myorg")
			Expect(err).NotTo(HaveOccurred())
			Expect(fc.PullRequest.BodyInstructions).To(Equal("Link the tracking issue."))
			Expect(*fc.PullRequest.ReportCost).To(BeTrue())
		})

		It("round-trips a forge's block through Put", func() {
			Expect(store.Put(ctx, &forgeconfig.ForgeConfig{
				Name: "gh", Type: "github",
				PullRequest: forgeconfig.PRContent{
					BodyInstructions: "Be specific.",
					CommentHeaders: forgeconfig.CommentHeaders{
						NewPullRequest: `{{ with index .Metadata "issue-id" }}**Issue:** {{ . }}{{ end }}`,
						FollowUpCommit: "Follow-up",
					},
					CommitTrailers: []string{"Kvarn-Mode: {{ .Mode }}"},
					ReportCost:       boolPtr(false),
				},
			})).To(Succeed())

			fc, err := store.Get(ctx, "gh")
			Expect(err).NotTo(HaveOccurred())
			Expect(fc.PullRequest.BodyInstructions).To(Equal("Be specific."))
			Expect(fc.PullRequest.CommentHeaders.NewPullRequest).To(
				Equal(`{{ with index .Metadata "issue-id" }}**Issue:** {{ . }}{{ end }}`))
			Expect(fc.PullRequest.CommentHeaders.FollowUpCommit).To(Equal("Follow-up"))
			Expect(fc.PullRequest.CommentHeaders.PRComment).To(BeEmpty())
			Expect(fc.PullRequest.CommitTrailers).To(Equal([]string{"Kvarn-Mode: {{ .Mode }}"}))
			Expect(*fc.PullRequest.ReportCost).To(BeFalse())
		})

		It("reads and round-trips the quote mode", func() {
			path := filepath.Join(tmpDir, "forges.toml")
			Expect(os.WriteFile(path, []byte(`[defaults.pull_request]
quote_task = "collapsed"
`), 0644)).To(Succeed())

			d, err := store.Defaults(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.PullRequest.QuoteTask).To(Equal(forgeconfig.QuoteCollapsed))

			Expect(store.Put(ctx, &forgeconfig.ForgeConfig{
				Name: "gh", Type: "github",
				PullRequest: forgeconfig.PRContent{QuoteTask: forgeconfig.QuoteOff},
			})).To(Succeed())

			fc, err := store.Get(ctx, "gh")
			Expect(err).NotTo(HaveOccurred())
			Expect(fc.PullRequest.QuoteTask).To(Equal(forgeconfig.QuoteOff))
		})

		It("fails the load on an unknown quote mode rather than ignoring it", func() {
			path := filepath.Join(tmpDir, "forges.toml")
			Expect(os.WriteFile(path, []byte(`[defaults.pull_request]
quote_task = "sometimes"
`), 0644)).To(Succeed())

			_, err := store.Defaults(ctx)
			Expect(err).To(MatchError(ContainSubstring("unknown quote mode")))
		})

		It("does not add an empty table for a forge that sets nothing", func() {
			path := filepath.Join(tmpDir, "forges.toml")
			Expect(store.Put(ctx, &forgeconfig.ForgeConfig{Name: "gh", Type: "github"})).To(Succeed())

			raw, err := os.ReadFile(path)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(raw)).NotTo(ContainSubstring("pull_request"))
		})
	})
})

func boolPtr(v bool) *bool { return &v }
