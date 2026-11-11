package forgegit_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/forge"
	forgegit "github.com/aholstenson/kvarn/internal/forge/git"
)

var _ = Describe("Git Forge", func() {
	var g *forgegit.Git

	BeforeEach(func() {
		g = forgegit.New()
	})

	Describe("ResolveCloneURL", func() {
		It("returns the input as-is for HTTPS URLs", func() {
			url, err := g.ResolveCloneURL("https://git.internal/team/tools.git")
			Expect(err).NotTo(HaveOccurred())
			Expect(url).To(Equal("https://git.internal/team/tools.git"))
		})

		It("returns the input as-is for SSH URLs", func() {
			url, err := g.ResolveCloneURL("git@git.internal:team/tools.git")
			Expect(err).NotTo(HaveOccurred())
			Expect(url).To(Equal("git@git.internal:team/tools.git"))
		})
	})

	Describe("ResolveCredentials", func() {
		It("passes through token", func() {
			src, err := g.ResolveCredentials(context.Background(), map[string]string{
				"token": "my-token",
			})
			Expect(err).NotTo(HaveOccurred())
			creds, err := src.Credentials(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(creds.Token).To(Equal("my-token"))
		})

		It("passes through SSH key path", func() {
			src, err := g.ResolveCredentials(context.Background(), map[string]string{
				"ssh_key_path": "/path/to/key",
				"ssh_key_pass": "passphrase",
			})
			Expect(err).NotTo(HaveOccurred())
			creds, err := src.Credentials(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(string(creds.SSHKey)).To(Equal("/path/to/key"))
			Expect(creds.SSHKeyPass).To(Equal("passphrase"))
		})

		It("passes through username/password", func() {
			src, err := g.ResolveCredentials(context.Background(), map[string]string{
				"username": "user",
				"password": "pass",
			})
			Expect(err).NotTo(HaveOccurred())
			creds, err := src.Credentials(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(creds.Username).To(Equal("user"))
			Expect(creds.Password).To(Equal("pass"))
		})

		It("handles empty config", func() {
			src, err := g.ResolveCredentials(context.Background(), map[string]string{})
			Expect(err).NotTo(HaveOccurred())
			Expect(src).NotTo(BeNil())
			creds, err := src.Credentials(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(creds).NotTo(BeNil())
		})
	})

	Describe("CreatePullRequest", func() {
		It("returns an error", func() {
			_, err := g.CreatePullRequest(context.Background(), forge.CreatePROpts{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not supported"))
		})
	})

	Describe("PostComment", func() {
		It("returns an error", func() {
			err := g.PostComment(context.Background(), forge.PostCommentOpts{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not supported"))
		})
	})

	Describe("delivery lookups", func() {
		It("refuses them with errors that are not retried", func() {
			_, err := g.FindPullRequest(context.Background(), forge.FindPROpts{})
			Expect(err).To(MatchError(ContainSubstring("not supported")))
			Expect(forge.IsTransient(err)).To(BeFalse())

			err = g.AddLabels(context.Background(), forge.AddLabelsOpts{})
			Expect(err).To(MatchError(ContainSubstring("not supported")))
			Expect(forge.IsTransient(err)).To(BeFalse())

			_, err = g.HasComment(context.Background(), forge.HasCommentOpts{})
			Expect(err).To(MatchError(ContainSubstring("not supported")))
			Expect(forge.IsTransient(err)).To(BeFalse())
		})
	})

	Describe("SCM", func() {
		It("returns a git SCM", func() {
			Expect(g.SCM()).NotTo(BeNil())
		})
	})
})
